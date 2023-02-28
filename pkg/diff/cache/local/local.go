// Copyright 2023 The Kelemetry Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package local

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	diffcache "github.com/kubewharf/kelemetry/pkg/diff/cache"
	"github.com/kubewharf/kelemetry/pkg/manager"
	"github.com/kubewharf/kelemetry/pkg/util"
	"github.com/kubewharf/kelemetry/pkg/util/cache"
	"github.com/kubewharf/kelemetry/pkg/util/shutdown"
)

func init() {
	manager.Global.ProvideMuxImpl("diff-cache/local", newLocal, diffcache.Cache.Store)
}

type localCache struct {
	manager.MuxImplBase

	logger logrus.FieldLogger

	data     map[string]*history
	dataLock sync.RWMutex

	snapshotCache *cache.TtlOnce
}

type history struct {
	lastModify time.Time
	patches    map[string]*diffcache.Patch
}

func newLocal(logger logrus.FieldLogger) *localCache {
	return &localCache{
		logger: logger,
		data:   map[string]*history{},
	}
}

func (_ *localCache) MuxImplName() (name string, isDefault bool) { return "local", true }

func (cache *localCache) Options() manager.Options { return &manager.NoOptions{} }

func (lc *localCache) Init(ctx context.Context) error {
	lc.snapshotCache = cache.NewTtlOnce(lc.GetCommonOptions().SnapshotTtl)
	return nil
}

func (cache *localCache) Start(stopCh <-chan struct{}) error {
	ttl := cache.GetAdditionalOptions().(*diffcache.CommonOptions).PatchTtl
	if ttl > 0 {
		go cache.runTrimLoop(ttl, time.Hour, stopCh)
	}

	go cache.snapshotCache.RunCleanupLoop(stopCh, cache.logger)

	return nil
}

func (cache *localCache) runTrimLoop(expiry time.Duration, interval time.Duration, stopCh <-chan struct{}) {
	logger := cache.logger.WithField("submod", "trimLoop")
	defer shutdown.RecoverPanic(logger)

	for {
		select {
		case <-stopCh:
			return
		case <-time.After(interval):
			cache.doTrim(expiry)
		}
	}
}

func (cache *localCache) doTrim(expiry time.Duration) {
	cache.dataLock.Lock()
	defer cache.dataLock.Unlock()

	removals := []string{}
	for k, v := range cache.data {
		if time.Since(v.lastModify) > expiry {
			removals = append(removals, k)
		}
	}

	for _, k := range removals {
		delete(cache.data, k)
	}
}

func (cache *localCache) Close() error { return nil }

func (cache *localCache) GetCommonOptions() *diffcache.CommonOptions {
	return cache.GetAdditionalOptions().(*diffcache.CommonOptions)
}

func (cache *localCache) Store(ctx context.Context, object util.ObjectRef, patch *diffcache.Patch) {
	cache.dataLock.Lock()
	defer cache.dataLock.Unlock()

	if _, exists := cache.data[object.String()]; !exists {
		cache.data[object.String()] = &history{patches: map[string]*diffcache.Patch{}}
	}

	patches := cache.data[object.String()]
	patches.lastModify = time.Now()

	keyRv, _ := cache.GetCommonOptions().ChooseResourceVersion(patch.OldResourceVersion, &patch.NewResourceVersion)
	patches.patches[keyRv] = patch
}

func (cache *localCache) Fetch(
	ctx context.Context,
	object util.ObjectRef,
	oldResourceVersion string,
	newResourceVersion *string,
) (*diffcache.Patch, error) {
	cache.dataLock.RLock()
	defer cache.dataLock.RUnlock()

	keyRv, err := cache.GetCommonOptions().ChooseResourceVersion(oldResourceVersion, newResourceVersion)
	if err != nil {
		return nil, err
	}

	history := cache.data[object.String()]
	if history != nil {
		patch, exists := history.patches[keyRv]
		if exists {
			return patch, nil
		}
	}

	keys := []string{}
	if history != nil {
		for k := range history.patches {
			keys = append(keys, k)
		}
	}

	cache.logger.WithField("object", object).Debugf("Cannot locate %v from %v", keyRv, keys)

	return nil, nil
}

func (cache *localCache) StoreSnapshot(ctx context.Context, object util.ObjectRef, snapshotName string, value *diffcache.Snapshot) {
	cache.snapshotCache.Add(fmt.Sprintf("%v/%s", object, snapshotName), value)
}

func (cache *localCache) FetchSnapshot(
	ctx context.Context,
	object util.ObjectRef,
	snapshotName string,
) (*diffcache.Snapshot, error) {
	if value, ok := cache.snapshotCache.Get(fmt.Sprintf("%v/%s", object, snapshotName)); ok {
		return value.(*diffcache.Snapshot), nil
	}

	return nil, nil
}

func (cache *localCache) List(ctx context.Context, object util.ObjectRef, limit int) ([]string, error) {
	cache.dataLock.RLock()
	defer cache.dataLock.RUnlock()

	history := cache.data[object.String()]
	if history == nil {
		return []string{}, nil
	}

	keys := []string{}
	for k := range history.patches {
		keys = append(keys, k)
	}

	return keys, nil
}
