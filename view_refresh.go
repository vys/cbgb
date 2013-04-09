package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"sync/atomic"
	"time"

	"github.com/couchbaselabs/walrus"
	"github.com/steveyen/gkvlite"
)

const (
	VIEWS_FILE_SUFFIX = "views"
)

var viewsRefresher *periodically

func (v *VBucket) markStale() {
	newval := atomic.AddInt64(&v.staleness, 1)
	if newval == 1 {
		viewsRefresher.Register(v.available, v.mkViewsRefreshFun())
	}
}

func (v *VBucket) mkViewsRefreshFun() func(time.Time) bool {
	return func(t time.Time) bool {
		return v.periodicViewsRefresh(t)
	}
}

func (v *VBucket) periodicViewsRefresh(time.Time) bool {
	leftovers, _ := v.viewsRefresh()
	return leftovers > 0
}

// Refreshes all views with the changes that happened since the last
// call to viewsRefresh().
func (v *VBucket) viewsRefresh() (int64, error) {
	v.viewsLock.Lock()
	defer v.viewsLock.Unlock()

	d := atomic.LoadInt64(&v.staleness)
	err := v.viewsRefresh_unlocked()
	if err != nil {
		return 0, err
	}
	return atomic.AddInt64(&v.staleness, -d), nil
}

func (v *VBucket) viewsRefresh_unlocked() error {
	ddocs := v.parent.GetDDocs()
	if ddocs == nil {
		return nil
	}
	viewsStore, err := v.getViewsStore()
	if err != nil {
		return err
	}
	backIndex := viewsStore.getPartitionStore(v.vbid)
	if backIndex == nil {
		return fmt.Errorf("missing back index store, vbid: %v", v.vbid)
	}
	_, backIndexChanges := backIndex.colls()

	// Need mutate() to be sync'ed with any compaction activity.
	var backIndexLastChange *gkvlite.Item
	backIndexLastChange, err = backIndexChanges.MaxItem(true)
	if err != nil {
		return err
	}
	var backIndexLastChangeBytes []byte
	var backIndexLastChangeNum uint64
	if backIndexLastChange != nil && len(backIndexLastChange.Key) > 0 {
		backIndexLastChangeBytes = backIndexLastChange.Key
		backIndexLastChangeNum, err = casBytesParse(backIndexLastChangeBytes)
		if err != nil {
			return err
		}
	}
	errVisit := v.ps.visitChanges(backIndexLastChangeBytes, true,
		func(i *item) bool {
			if len(i.key) == 0 { // An empty key == metadata change.
				return true
			}
			if i.cas <= backIndexLastChangeNum {
				return true
			}
			err = v.viewsRefreshItem(ddocs, viewsStore, backIndex, i)
			if err != nil {
				return false
			}
			return true
		})
	if errVisit != nil {
		return errVisit
	}
	return err
}

// Refreshes all views w.r.t. a single item/doc.
func (v *VBucket) viewsRefreshItem(ddocs *DDocs,
	viewsStore *bucketstore, backIndex *partitionstore, i *item) error {
	oldBackIndexItem, err := backIndex.get(i.key)
	if err != nil {
		return err
	}
	// TODO: One day do view hashing so view indexes are shared.
	viewEmits := map[string]ViewRows{} // Keyed by "ddocId/viewId".
	for ddocId, ddoc := range *ddocs {
		for viewId, view := range ddoc.Views {
			emits, err := v.execViewMapFunction(ddocId, ddoc, viewId, view, i)
			if err != nil {
				return err
			}
			viewEmits[ddocId+"/"+viewId] = emits
		}
	}
	j, err := json.Marshal(viewEmits)
	if err != nil {
		return err
	}
	newBackIndexItem := &item{
		key:  i.key,
		cas:  i.cas,
		data: j,
	}
	// TODO: Track size of backIndex as set() returns deltaItemBytes.
	_, errSet := backIndex.setWithCallback(newBackIndexItem, oldBackIndexItem,
		func() {
			if oldBackIndexItem != nil {
				var viewEmitsOld map[string]ViewRows
				err = json.Unmarshal(oldBackIndexItem.data, &viewEmitsOld)
				if err != nil {
					return
				}
				err = vindexesClear(viewsStore, i.key, viewEmitsOld)
				if err != nil {
					return
				}
			}
			err = vindexesSet(viewsStore, i.key, viewEmits)
		})
	if errSet != nil {
		return errSet
	}
	return err
}

// Executes the map function on an item.
func (v *VBucket) execViewMapFunction(ddocId string, ddoc *DDoc,
	viewId string, view *View, i *item) (ViewRows, error) {
	pvmf, err := view.GetViewMapFunction()
	if err != nil {
		return nil, err
	}
	docId := string(i.key)
	docType := "json"
	var doc interface{}
	err = json.Unmarshal(i.data, &doc)
	if err != nil {
		doc = base64.StdEncoding.EncodeToString(i.data)
		docType = "base64"
	}
	odoc, err := OttoFromGo(pvmf.otto, doc)
	if err != nil {
		return nil, err
	}
	meta := map[string]interface{}{
		"id":   docId,
		"type": docType,
	}
	ometa, err := OttoFromGo(pvmf.otto, meta)
	if err != nil {
		return nil, err
	}
	_, err = pvmf.mapf.Call(pvmf.mapf, odoc, ometa)
	if err != nil {
		return nil, err
	}
	emits, err := pvmf.restartEmits()
	if err != nil {
		return nil, err
	}
	for _, emit := range emits {
		emit.Id = docId
	}
	return emits, nil
}

func (v *VBucket) getViewsStore() (res *bucketstore, err error) {
	v.Apply(func() {
		if v.viewsStore == nil {
			dirForBucket := v.parent.GetBucketDir()
			settings := v.parent.GetBucketSettings()
			vfprefix := fmt.Sprintf("%s_%d", settings.UUID, v.vbid)
			var vfn string
			if settings.MemoryOnly < MemoryOnly_LEVEL_PERSIST_NOTHING {
				vfn, err = latestStoreFileName(dirForBucket, vfprefix, VIEWS_FILE_SUFFIX)
				if err != nil {
					return
				}
			} else {
				vfn = makeStoreFileName(vfprefix, 0, VIEWS_FILE_SUFFIX)
			}
			p := path.Join(dirForBucket, vfn)
			v.viewsStore, err = newBucketStore(p, *settings)
		}
		res = v.viewsStore
	})
	return res, err
}

// Used to deletes previous emits from the vindexes.
func vindexesClear(viewsStore *bucketstore, docId []byte,
	viewEmits map[string]ViewRows) error {
	for vindexName, emits := range viewEmits {
		vindex := viewsStore.collWithKeyCompare(vindexName, vindexKeyCompare)
		for _, emit := range emits {
			vk, err := vindexKey(docId, emit.Key)
			if err != nil {
				return err
			}
			_, err = vindex.Delete(vk)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Used to incorporate emits into the vindexes.
func vindexesSet(viewsStore *bucketstore, docId []byte,
	viewEmits map[string]ViewRows) error {
	for vindexName, emits := range viewEmits {
		vindex := viewsStore.collWithKeyCompare(vindexName, vindexKeyCompare)
		for _, emit := range emits {
			j, err := json.Marshal(emit.Value)
			if err != nil {
				return err
			}
			vk, err := vindexKey(docId, emit.Key)
			if err != nil {
				return err
			}
			err = vindex.Set(vk, j)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func vindexKeyCompare(a, b []byte) int {
	docIdA, emitKeyA, err := vindexKeyParse(a)
	if err != nil {
		return bytes.Compare(a, b)
	}
	docIdB, emitKeyB, err := vindexKeyParse(b)
	if err != nil {
		return bytes.Compare(a, b)
	}
	c := walrus.CollateJSON(emitKeyA, emitKeyB)
	if c == 0 {
		return bytes.Compare(docIdA, docIdB)
	}
	return c
}

// Returns byte array that looks like "docId/emitKey".
func vindexKey(docId []byte, emitKey interface{}) ([]byte, error) {
	emitKeyBytes, err := json.Marshal(emitKey)
	if err != nil {
		return nil, err
	}
	return bytes.Join([][]byte{emitKeyBytes, docId}, []byte{0}), nil
}

func vindexKeyParse(k []byte) (docId []byte, emitKey interface{}, err error) {
	parts := bytes.Split(k, []byte{0})
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("vindexKeyParse failed split: %v", k)
	}
	if err = json.Unmarshal(parts[0], &emitKey); err != nil {
		return nil, nil, err
	}
	return parts[1], emitKey, nil
}
