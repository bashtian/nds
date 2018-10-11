package nds

import (
	"context"
	"reflect"
	"sync"

	"cloud.google.com/go/datastore"
	"github.com/pkg/errors"
)

// putMultiLimit is the Google Cloud Datastore limit for the maximum number
// of entities that can be put by datastore.PutMulti at once.
// // https://cloud.google.com/datastore/docs/concepts/limits
const putMultiLimit = 500

var (
	// This exists purely for testing
	putMultiHook func() error
)

// PutMulti is a batch version of Put. It works just like datastore.PutMulti
// except it interacts appropriately with NDS's caching strategy. It also
// removes the API limit of 500 entities per request by calling the datastore as
// many times as required to put all the keys. It does this efficiently and
// concurrently.
func (c *Client) PutMulti(ctx context.Context,
	keys []*datastore.Key, vals interface{}) ([]*datastore.Key, error) {

	if len(keys) == 0 {
		return nil, nil
	}

	v := reflect.ValueOf(vals)
	if err := checkKeysValues(keys, v); err != nil {
		return nil, err
	}

	callCount := (len(keys)-1)/putMultiLimit + 1
	putKeys := make([][]*datastore.Key, callCount)
	errs := make([]error, callCount)

	var wg sync.WaitGroup
	wg.Add(callCount)
	for i := 0; i < callCount; i++ {
		lo := i * putMultiLimit
		hi := (i + 1) * putMultiLimit
		if hi > len(keys) {
			hi = len(keys)
		}

		go func(i int, keys []*datastore.Key, vals reflect.Value) {
			putKeys[i], errs[i] = c.putMulti(ctx, keys, vals.Interface())
			wg.Done()
		}(i, keys[lo:hi], v.Slice(lo, hi))
	}
	wg.Wait()

	if isErrorsNil(errs) {
		groupedKeys := make([]*datastore.Key, len(keys))
		for i, k := range putKeys {
			lo := i * putMultiLimit
			hi := (i + 1) * putMultiLimit
			if hi > len(keys) {
				hi = len(keys)
			}
			copy(groupedKeys[lo:hi], k)
		}
		return groupedKeys, nil
	}

	groupedKeys := make([]*datastore.Key, len(keys))
	groupedErrs := make(datastore.MultiError, len(keys))
	for i, err := range errs {
		lo := i * putMultiLimit
		hi := (i + 1) * putMultiLimit
		if hi > len(keys) {
			hi = len(keys)
		}
		if me, ok := err.(datastore.MultiError); ok {
			for j, e := range me {
				if e == nil {
					groupedKeys[lo+j] = putKeys[i][j]
				} else {
					groupedErrs[lo+j] = e
				}
			}
		} else if err != nil {
			for j := lo; j < hi; j++ {
				groupedErrs[j] = err
			}
		}
	}

	return groupedKeys, groupedErrs
}

// Put saves the entity val into the datastore with key. val must be a struct
// pointer; if a struct pointer then any unexported fields of that struct will
// be skipped. If key is an incomplete key, the returned key will be a unique
// key generated by the datastore.
func (c *Client) Put(ctx context.Context,
	key *datastore.Key, val interface{}) (*datastore.Key, error) {

	keys := []*datastore.Key{key}
	vals := []interface{}{val}
	if err := checkKeysValues(keys, reflect.ValueOf(vals)); err != nil {
		return nil, err
	}

	keys, err := c.putMulti(ctx, keys, vals)
	switch e := err.(type) {
	case nil:
		return keys[0], nil
	case datastore.MultiError:
		return nil, e[0]
	default:
		return nil, err
	}
}

// putMulti locks the items in cache, puts the entities into the datastore, and then deletes the locks in cache.
func (c *Client) putMulti(ctx context.Context,
	keys []*datastore.Key, vals interface{}) ([]*datastore.Key, error) {
	lockCacheKeys, lockCacheItems := getCacheLocks(keys)

	cacheCtx, err := c.cacher.NewContext(ctx)
	if err != nil {
		return nil, err
	}

	defer func() {
		// Remove the locks.
		if err := c.cacher.DeleteMulti(cacheCtx,
			lockCacheKeys); err != nil {
			c.onError(ctx, errors.Wrap(err, "putMulti cache.DeleteMulti"))
		}
	}()

	if err := c.cacher.SetMulti(cacheCtx,
		lockCacheItems); err != nil {
		return nil, err
	}

	if putMultiHook != nil {
		if err := putMultiHook(); err != nil {
			return keys, err
		}
	}

	// Save to the datastore.
	return c.ds.PutMulti(ctx, keys, vals)
}
