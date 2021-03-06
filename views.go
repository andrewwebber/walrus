package walrus

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// A single view stored in a lolrus.
type lolrusView struct {
	mapFunction         *JSMapFunction // The compiled map function
	reduceFunction      string         // The source of the reduce function (if any)
	index               ViewResult     // The latest complete result
	lastIndexedSequence uint64         // Bucket's lastSeq at the time the index was built
}

// Stores view functions for use by a lolrus.
type lolrusDesignDoc map[string]*lolrusView

func (bucket *lolrus) GetDDoc(docname string, into interface{}) error {
	bucket.lock.Lock()
	defer bucket.lock.Unlock()

	design := bucket.DesignDocs[docname]
	if design == nil {
		return MissingError{docname}
	}
	// Have to roundtrip thru JSON to return it as arbitrary interface{}:
	raw, _ := json.Marshal(design)
	return json.Unmarshal(raw, into)
}

func (bucket *lolrus) PutDDoc(docname string, value interface{}) error {
	design, err := CheckDDoc(value)
	if err != nil {
		return err
	}

	bucket.lock.Lock()
	defer bucket.lock.Unlock()

	if reflect.DeepEqual(design, bucket.DesignDocs[docname]) {
		return nil // unchanged
	}

	err = bucket._compileDesignDoc(docname, design)
	if err != nil {
		return err
	}

	bucket.DesignDocs[docname] = design
	bucket._saveSoon()
	return nil
}

func (bucket *lolrus) DeleteDDoc(docname string) error {
	bucket.lock.Lock()
	defer bucket.lock.Unlock()

	if bucket.DesignDocs[docname] == nil {
		return MissingError{docname}
	}
	delete(bucket.DesignDocs, docname)
	delete(bucket.views, docname)
	return nil
}

func (bucket *lolrus) _compileDesignDoc(docname string, design *DesignDoc) error {
	if design == nil {
		return nil
	}
	ddoc := lolrusDesignDoc{}
	for name, fns := range design.Views {
		jsserver := NewJSMapFunction(fns.Map)
		view := &lolrusView{
			mapFunction:    jsserver,
			reduceFunction: fns.Reduce,
		}
		ddoc[name] = view
	}
	bucket.views[docname] = ddoc
	return nil
}

// Validates a design document.
func CheckDDoc(value interface{}) (*DesignDoc, error) {
	source, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	var design DesignDoc
	if err := json.Unmarshal(source, &design); err != nil {
		return nil, err
	}

	if design.Language != "" && design.Language != "javascript" {
		return nil, fmt.Errorf("Lolrus design docs don't support language %q",
			design.Language)
	}

	return &design, nil
}

// Looks up a lolrusView, and its current index if it's up-to-date enough.
func (bucket *lolrus) findView(docName, viewName string, staleOK bool) (view *lolrusView, result *ViewResult) {
	bucket.lock.RLock()
	defer bucket.lock.RUnlock()

	if ddoc, exists := bucket.views[docName]; exists {
		view = ddoc[viewName]
		if view != nil {
			upToDate := view.lastIndexedSequence == bucket.LastSeq
			if !upToDate && view.lastIndexedSequence > 0 && staleOK {
				go bucket.updateView(view, bucket.LastSeq)
				upToDate = true
			}
			if upToDate {
				curResult := view.index // copy the struct
				result = &curResult
			}
		}
	}
	return
}

func (bucket *lolrus) View(docName, viewName string, params map[string]interface{}) (ViewResult, error) {
	// Note: This method itself doesn't lock, so it shouldn't access bucket fields directly.
	ohai("View(%q, %q) ...", docName, viewName)

	stale := true
	if params != nil {
		if staleParam, found := params["stale"].(bool); found {
			stale = staleParam
		}
	}

	// Look up the view and its index:
	var result ViewResult
	view, resultMaybe := bucket.findView(docName, viewName, stale)
	if view == nil {
		return result, bucket.missingError(docName + "/" + viewName)
	} else if resultMaybe != nil {
		result = *resultMaybe
	} else {
		result = bucket.updateView(view, 0)
	}

	return ProcessViewResult(result, params, bucket, view.reduceFunction)
}

// Updates the view index if necessary, and returns it.
func (bucket *lolrus) updateView(view *lolrusView, toSequence uint64) ViewResult {
	bucket.lock.Lock()
	defer bucket.lock.Unlock()

	if toSequence == 0 {
		toSequence = bucket.LastSeq
	}
	if view.lastIndexedSequence >= toSequence {
		return view.index
	}
	ohai("\t... updating index to seq %d (from %d)", toSequence, view.lastIndexedSequence)

	var result ViewResult
	result.Rows = make([]*ViewRow, 0, len(bucket.Docs))
	result.Errors = make([]ViewError, 0)

	updatedKeysSize := toSequence - view.lastIndexedSequence
	if updatedKeysSize > 1000 {
		updatedKeysSize = 1000
	}
	updatedKeys := make(map[string]struct{}, updatedKeysSize)

	// Build a parallel task to map docs:
	mapFunction := view.mapFunction
	mapper := func(rawInput interface{}, output chan<- interface{}) {
		input := rawInput.([2]string)
		docid := input[0]
		raw := input[1]
		rows, err := mapFunction.CallFunction(string(raw), docid)
		if err != nil {
			ohai("Error running map function: %s", err)
			output <- ViewError{docid, err.Error()}
		} else {
			output <- rows
		}
	}
	mapInput := make(chan interface{})
	mapOutput := Parallelize(mapper, 0, mapInput)

	// Start another task to read the map output and store it into result.Rows/Errors:
	var waiter sync.WaitGroup
	waiter.Add(1)
	go func() {
		defer waiter.Done()
		for item := range mapOutput {
			switch item := item.(type) {
			case ViewError:
				result.Errors = append(result.Errors, item)
			case []*ViewRow:
				result.Rows = append(result.Rows, item...)
			}
		}
	}()

	// Now shovel all the changed document bodies into the mapper:
	for docid, doc := range bucket.Docs {
		if doc.Sequence > view.lastIndexedSequence {
			raw := doc.Raw
			if raw != nil {
				if !doc.IsJSON {
					raw = []byte(`{}`) // Ignore contents of non-JSON (raw) docs
				}
				mapInput <- [2]string{docid, string(raw)}
				updatedKeys[docid] = struct{}{}
			}
		}
	}
	close(mapInput)

	// Wait for the result processing to finish:
	waiter.Wait()

	// Copy existing view rows emitted by unchanged docs:
	for _, row := range view.index.Rows {
		if _, found := updatedKeys[row.ID]; !found {
			result.Rows = append(result.Rows, row)
		}
	}
	for _, err := range view.index.Errors {
		if _, found := updatedKeys[err.From]; !found {
			result.Errors = append(result.Errors, err)
		}
	}

	sort.Sort(&result)
	result.collator.Clear() // don't keep collation state around

	view.lastIndexedSequence = bucket.LastSeq
	view.index = result
	return view.index
}

func (bucket *lolrus) ViewCustom(ddoc, name string, params map[string]interface{}, vres interface{}) error {
	result, err := bucket.View(ddoc, name, params)
	if err != nil {
		return err
	}
	marshaled, _ := json.Marshal(result)
	return json.Unmarshal(marshaled, vres)
}

// Applies view params (startkey/endkey, limit, etc) against a ViewResult.
func ProcessViewResult(result ViewResult, params map[string]interface{},
	bucket Bucket, reduceFunction string) (ViewResult, error) {
	includeDocs := false
	limit := 0
	reverse := false
	reduce := true

	if params != nil {
		includeDocs, _ = params["include_docs"].(bool)
		limit, _ = params["limit"].(int)
		reverse, _ = params["reverse"].(bool)
		if reduceParam, found := params["reduce"].(bool); found {
			reduce = reduceParam
		}
	}

	if reverse {
		//TODO: Apply "reverse" option
		return result, fmt.Errorf("Reverse is not supported yet, sorry")
	}

	startkey := params["startkey"]
	if startkey == nil {
		startkey = params["start_key"] // older synonym
	}
	endkey := params["endkey"]
	if endkey == nil {
		endkey = params["end_key"]
	}
	inclusiveEnd := true
	if key := params["key"]; key != nil {
		startkey = key
		endkey = key
	} else {
		if value, ok := params["inclusive_end"].(bool); ok {
			inclusiveEnd = value
		}
	}

	var collator JSONCollator

	if startkey != nil {
		i := sort.Search(len(result.Rows), func(i int) bool {
			return collator.Collate(result.Rows[i].Key, startkey) >= 0
		})
		result.Rows = result.Rows[i:]
	}

	if limit > 0 && len(result.Rows) > limit {
		result.Rows = result.Rows[:limit]
	}

	if endkey != nil {
		limit := 0
		if !inclusiveEnd {
			limit = -1
		}
		i := sort.Search(len(result.Rows), func(i int) bool {
			return collator.Collate(result.Rows[i].Key, endkey) > limit
		})
		result.Rows = result.Rows[:i]
	}

	if includeDocs {
		newRows := make(ViewRows, len(result.Rows))
		for i, row := range result.Rows {
			//OPT: This may unmarshal the same doc more than once
			raw, err := bucket.GetRaw(row.ID)
			if err != nil {
				return result, err
			}
			var parsedDoc interface{}
			json.Unmarshal(raw, &parsedDoc)
			newRows[i] = row
			newRows[i].Doc = &parsedDoc
		}
		result.Rows = newRows
	}

	if reduce && reduceFunction != "" {
		if err := ReduceViewResult(reduceFunction, &result); err != nil {
			return result, err
		}
	}

	result.TotalRows = len(result.Rows)
	ohai("\t... view returned %d rows", result.TotalRows)
	return result, nil
}

func ReduceViewResult(reduceFunction string, result *ViewResult) error {
	switch reduceFunction {
	case "_count":
		result.Rows = []*ViewRow{{Value: float64(len(result.Rows))}}
		return nil
	default:
		// TODO: Implement other reduce functions!
		return fmt.Errorf("Walrus only supports _count reduce function")
	}
}

//////// VIEW RESULT: (implementation of sort.Interface interface)

func (result *ViewResult) Len() int {
	return len(result.Rows)
}

func (result *ViewResult) Swap(i, j int) {
	temp := result.Rows[i]
	result.Rows[i] = result.Rows[j]
	result.Rows[j] = temp
}

func (result *ViewResult) Less(i, j int) bool {
	return result.collator.Collate(result.Rows[i].Key, result.Rows[j].Key) < 0
}

//////// DUMP:

func (bucket *lolrus) _sortedKeys() []string {
	keys := make([]string, 0, len(bucket.Docs))
	for key, _ := range bucket.Docs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (bucket *lolrus) Dump() {
	bucket.lock.RLock()
	defer bucket.lock.RUnlock()
	fmt.Printf("==== Walrus bucket %q\n", bucket.name)
	for _, key := range bucket._sortedKeys() {
		doc := bucket.Docs[key]
		fmt.Printf("   %q = ", key)
		if doc.IsJSON {
			fmt.Println(string(doc.Raw))
		} else {
			fmt.Printf("<%d bytes>\n", len(doc.Raw))
		}
	}
	fmt.Printf("==== End bucket %q\n", bucket.name)
}
