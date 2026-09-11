//go:build !nodms

/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	dms "github.com/lelezi257/dms/sdk/go"
)

func TestDMSGetDistinguishesMissingEmptyAndFailure(t *testing.T) {
	t.Run("returns not exist when SDK misses whole object", func(t *testing.T) {
		store := newTestDMSStore(&mockDMSClient{
			getFunc: func(context.Context, string) ([]byte, bool, error) {
				return nil, false, nil
			},
		})

		_, err := store.Get(context.Background(), "missing", 0, -1)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected os.ErrNotExist, got %v", err)
		}
	})

	t.Run("returns empty reader when SDK finds empty object", func(t *testing.T) {
		store := newTestDMSStore(&mockDMSClient{
			getFunc: func(context.Context, string) ([]byte, bool, error) {
				return []byte{}, true, nil
			},
		})

		data := readDMSObject(t, store, "empty", 0, -1)
		if data != "" {
			t.Fatalf("expected empty payload, got %q", data)
		}
	})

	t.Run("returns SDK error instead of not exist", func(t *testing.T) {
		want := errors.New("dms unavailable")
		store := newTestDMSStore(&mockDMSClient{
			getFunc: func(context.Context, string) ([]byte, bool, error) {
				return nil, false, want
			},
		})

		_, err := store.Get(context.Background(), "error", 0, -1)
		if !errors.Is(err, want) {
			t.Fatalf("expected SDK error %v, got %v", want, err)
		}
	})
}

func TestDMSRangeGetUsesOneClampedCurrentRead(t *testing.T) {
	client := &mockDMSClient{
		statFunc: func(context.Context, string) (dms.ObjectInfo, bool, error) {
			t.Fatal("range GET must not perform Stat")
			return dms.ObjectInfo{}, false, nil
		},
		getWithOptionsFunc: func(_ context.Context, key string, options dms.GetOptions) (dms.GetResult, bool, error) {
			if key != "range" {
				t.Fatalf("unexpected key: %q", key)
			}
			want := dms.GetOptions{Range: &dms.ByteRange{Offset: 2, Len: 3}, ClampRange: true}
			if !reflect.DeepEqual(options, want) {
				t.Fatalf("unexpected get options: got %#v want %#v", options, want)
			}
			return dms.GetResult{Version: 7, Bytes: []byte("234")}, true, nil
		},
	}
	store := newTestDMSStore(client)

	data := readDMSObject(t, store, "range", 2, 3)
	if data != "234" {
		t.Fatalf("expected range payload 234, got %q", data)
	}
	if client.getCalls() != 0 {
		t.Fatalf("range get must not use whole-object Get, got %d calls", client.getCalls())
	}
}

func TestDMSRangeGetClampsTailEndAndInvalidRanges(t *testing.T) {
	cases := []struct {
		name      string
		off       int64
		limit     int64
		wantRange dms.ByteRange
		wantData  string
	}{
		{name: "tail when limit is zero", off: 3, limit: 0, wantRange: dms.ByteRange{Offset: 3, Len: math.MaxUint64 - 3}, wantData: "lo"},
		{name: "clamps limit past end", off: 4, limit: 99, wantRange: dms.ByteRange{Offset: 4, Len: 99}, wantData: "o"},
		{name: "clamps offset past end", off: 9, limit: 2, wantRange: dms.ByteRange{Offset: 9, Len: 2}, wantData: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockDMSClient{
				statFunc: func(context.Context, string) (dms.ObjectInfo, bool, error) {
					return dms.ObjectInfo{Key: "hello", Len: 5, Version: 11}, true, nil
				},
				getWithOptionsFunc: func(_ context.Context, _ string, options dms.GetOptions) (dms.GetResult, bool, error) {
					want := dms.GetOptions{Range: &tc.wantRange, ClampRange: true}
					if !reflect.DeepEqual(options, want) {
						t.Fatalf("unexpected get options: got %#v want %#v", options, want)
					}
					return dms.GetResult{Version: 11, Bytes: []byte(tc.wantData)}, true, nil
				},
			}
			store := newTestDMSStore(client)

			data := readDMSObject(t, store, "hello", tc.off, tc.limit)
			if data != tc.wantData {
				t.Fatalf("unexpected payload: got %q want %q", data, tc.wantData)
			}
		})
	}

	store := newTestDMSStore(&mockDMSClient{})
	for _, tc := range []struct {
		name  string
		off   int64
		limit int64
	}{
		{name: "negative offset", off: -1, limit: 1},
		{name: "positive offset with negative limit", off: 1, limit: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Get(context.Background(), "invalid", tc.off, tc.limit)
			if err == nil {
				t.Fatalf("expected invalid range error")
			}
		})
	}
}

func TestDMSRangeGetLeavesVersionSelectionToSingleSDKRead(t *testing.T) {
	client := &mockDMSClient{
		statFunc: func(context.Context, string) (dms.ObjectInfo, bool, error) {
			t.Fatal("version and length must come from the single SDK read")
			return dms.ObjectInfo{}, false, nil
		},
		getWithOptionsFunc: func(_ context.Context, _ string, options dms.GetOptions) (dms.GetResult, bool, error) {
			want := dms.GetOptions{Range: &dms.ByteRange{Offset: 1, Len: 3}, ClampRange: true}
			if !reflect.DeepEqual(options, want) {
				t.Fatalf("range read did not ask Node for same-version clipping: got %#v want %#v", options, want)
			}
			return dms.GetResult{Version: 31, Bytes: []byte("old")}, true, nil
		},
	}
	store := newTestDMSStore(client)

	data := readDMSObject(t, store, "moving", 1, 3)
	if data != "old" {
		t.Fatalf("expected exact-version bytes, got %q", data)
	}
}

func TestDMSPutDoesNotSetOnReaderErrorOrOversize(t *testing.T) {
	t.Run("reader error", func(t *testing.T) {
		client := &mockDMSClient{}
		store := newTestDMSStore(client)

		err := store.Put(context.Background(), "broken", failingReader{err: errors.New("reader failed")})
		if err == nil || err.Error() != "reader failed" {
			t.Fatalf("expected reader error, got %v", err)
		}
		if client.setCalls() != 0 {
			t.Fatalf("reader error must not call Set, got %d calls", client.setCalls())
		}
	})

	t.Run("8MiB integration limit", func(t *testing.T) {
		client := &mockDMSClient{}
		store := newTestDMSStore(client)

		err := store.Put(context.Background(), "too-large", bytes.NewReader(make([]byte, dmsObjectLimit+1)))
		if err == nil {
			t.Fatalf("expected oversize error")
		}
		if client.setCalls() != 0 {
			t.Fatalf("oversize object must not call Set, got %d calls", client.setCalls())
		}
	})
}

func TestDMSPutRespectsCancelledWriterBudget(t *testing.T) {
	client := &mockDMSClient{}
	store := newTestDMSStore(client)
	store.writers <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.Put(ctx, "blocked", bytes.NewReader([]byte("value")))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled while waiting for writer budget, got %v", err)
	}
	if client.setCalls() != 0 {
		t.Fatalf("cancelled budget wait must not call Set, got %d calls", client.setCalls())
	}
}

func TestDMSHeadReturnsStableMtimeFromStat(t *testing.T) {
	mtime := time.Unix(1700000000, 42)
	store := newTestDMSStore(&mockDMSClient{
		statFunc: func(context.Context, string) (dms.ObjectInfo, bool, error) {
			return dms.ObjectInfo{Key: "head-key", Len: 19, Version: 5, ModifiedTime: mtime}, true, nil
		},
	})

	object, err := store.Head(context.Background(), "head-key")
	if err != nil {
		t.Fatalf("Head failed: %v", err)
	}
	if object.Key() != "head-key" || object.Size() != 19 || !object.Mtime().Equal(mtime) {
		t.Fatalf("unexpected object: key=%q size=%d mtime=%s", object.Key(), object.Size(), object.Mtime())
	}
}

func TestDMSListUsesStartAfterOnFirstPageAndCursorOnContinuation(t *testing.T) {
	mtime := time.Unix(10, 0)
	var calls []dms.ScanOptions
	client := &mockDMSClient{
		scanFunc: func(_ context.Context, prefix string, options dms.ScanOptions) (dms.ScanResult, error) {
			if prefix != "p/" {
				t.Fatalf("unexpected prefix: %q", prefix)
			}
			calls = append(calls, options)
			switch len(calls) {
			case 1:
				if options.StartAfter == nil || *options.StartAfter != "p/a" || options.Cursor != "" || options.Limit != 2 {
					t.Fatalf("unexpected first page options: %#v", options)
				}
				return dms.ScanResult{
					Items:      []dms.ObjectInfo{{Key: "p/b", Len: 2, ModifiedTime: mtime}},
					NextCursor: "next",
				}, nil
			case 2:
				if options.StartAfter != nil || options.Cursor != "next" || options.Limit != 1000 {
					t.Fatalf("unexpected continuation options: %#v", options)
				}
				return dms.ScanResult{Items: []dms.ObjectInfo{{Key: "p/c", Len: 3, ModifiedTime: mtime}}}, nil
			default:
				t.Fatalf("unexpected extra Scan call")
			}
			return dms.ScanResult{}, nil
		},
	}
	store := newTestDMSStore(client)

	page, more, token, err := store.List(context.Background(), "p/", "p/a", "", "", 2, true)
	if err != nil {
		t.Fatalf("first List failed: %v", err)
	}
	if !more || token != "next" || len(page) != 1 || page[0].Key() != "p/b" || page[0].Size() != 2 {
		t.Fatalf("unexpected first page: more=%v token=%q page=%v", more, token, keysOf(page))
	}
	page, more, token, err = store.List(context.Background(), "p/", "", token, "", 5000, true)
	if err != nil {
		t.Fatalf("continuation List failed: %v", err)
	}
	if more || token != "" || len(page) != 1 || page[0].Key() != "p/c" || page[0].Size() != 3 {
		t.Fatalf("unexpected continuation page: more=%v token=%q page=%v", more, token, keysOf(page))
	}
}

func TestDMSListRejectsNonPositiveLimit(t *testing.T) {
	store := newTestDMSStore(&mockDMSClient{})

	if _, _, _, err := store.List(context.Background(), "p/", "", "", "", 0, true); err == nil {
		t.Fatalf("expected non-positive limit error")
	}
}

func TestDMSListGroupsWithoutHeadAndCursorSupersedesMarker(t *testing.T) {
	store := newTestDMSStore(&mockDMSClient{
		statFunc: func(context.Context, string) (dms.ObjectInfo, bool, error) {
			t.Fatal("List must not Head entries")
			return dms.ObjectInfo{}, false, nil
		},
		scanFunc: func(_ context.Context, prefix string, options dms.ScanOptions) (dms.ScanResult, error) {
			if prefix != "p/" || options.Delimiter != "/" || options.Cursor != "cursor" || options.StartAfter != nil {
				t.Fatalf("bad options: %#v", options)
			}
			return dms.ScanResult{Items: []dms.ObjectInfo{{Key: "p/dir/", IsPrefix: true}}}, nil
		},
	})
	items, more, _, err := store.List(context.Background(), "p/", "old-marker", "cursor", "/", 1, true)
	if err != nil || more || len(items) != 1 || !items[0].IsDir() {
		t.Fatalf("group result %v %v %v", items, more, err)
	}
}

func TestDMSObjectDirSemanticsMatchS3SlashSuffix(t *testing.T) {
	group, err := dmsObject(dms.ObjectInfo{Key: "p/common/", IsPrefix: true})
	if err != nil {
		t.Fatal(err)
	}
	if !group.IsDir() {
		t.Fatalf("synthetic common prefix should be a directory: %q", group.Key())
	}

	realSlashKey, err := dmsObject(dms.ObjectInfo{Key: "p/dir/", IsPrefix: false})
	if err != nil {
		t.Fatal(err)
	}
	if !realSlashKey.IsDir() {
		t.Fatalf("real slash-suffixed object should follow S3 directory semantics: %q", realSlashKey.Key())
	}

	regular, err := dmsObject(dms.ObjectInfo{Key: "p/file", IsPrefix: false})
	if err != nil {
		t.Fatal(err)
	}
	if regular.IsDir() {
		t.Fatalf("regular object should not be a directory: %q", regular.Key())
	}
}

func TestDMSPutPassesKnownReaderWithoutMaterialization(t *testing.T) {
	input := bytes.NewReader([]byte("prefixVALUE"))
	_, _ = input.Seek(6, io.SeekStart)
	calls := 0
	store := newTestDMSStore(&mockDMSClient{setFromFunc: func(_ context.Context, key string, reader io.Reader, length uint64, _ dms.SetOptions) (dms.SetResult, error) {
		calls++
		wrapped, ok := reader.(dmsContextReader)
		if !ok || wrapped.source != input || length != 5 {
			t.Fatalf("input was copied or remaining size changed: %T %d", reader, length)
		}
		data, err := io.ReadAll(reader)
		if err != nil || string(data) != "VALUE" {
			t.Fatalf("bad input %q %v", data, err)
		}
		return dms.SetResult{Len: length}, nil
	}})
	if err := store.Put(context.Background(), "key", input); err != nil || calls != 1 {
		t.Fatalf("put calls=%d err=%v", calls, err)
	}
}

type closeCountingReader struct {
	io.Reader
	closed int
}

func (r *closeCountingReader) Close() error { r.closed++; return nil }

func TestDMSGetReturnsSDKReaderAndClosesIt(t *testing.T) {
	body := &closeCountingReader{Reader: bytes.NewReader([]byte("abc"))}
	calls := 0
	store := newTestDMSStore(&mockDMSClient{getReaderFunc: func(_ context.Context, _ string, _ dms.GetOptions) (dms.ReadResult, bool, error) {
		calls++
		return dms.ReadResult{Version: 7, Len: 3, Body: body}, true, nil
	}})
	r, err := store.Get(context.Background(), "k", 0, -1)
	if err != nil || r != body {
		t.Fatalf("SDK body must be returned directly %T %v", r, err)
	}
	buf := make([]byte, 1)
	_, _ = r.Read(buf)
	_, _ = r.Read(buf)
	_ = r.Close()
	if calls != 1 || body.closed != 1 {
		t.Fatalf("calls=%d closed=%d", calls, body.closed)
	}
}

func TestDMSListAllSendsNilSentinelOnMidStreamError(t *testing.T) {
	scanErr := errors.New("scan failed")
	client := &mockDMSClient{}
	client.scanFunc = func(_ context.Context, _ string, options dms.ScanOptions) (dms.ScanResult, error) {
		if options.Cursor == "" {
			return dms.ScanResult{
				Items:      []dms.ObjectInfo{{Key: "p/first", Len: 1}},
				NextCursor: "next",
			}, nil
		}
		return dms.ScanResult{}, scanErr
	}
	store := newTestDMSStore(client)

	ch, err := store.ListAll(context.Background(), "p/", "", true)
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	first := <-ch
	if first == nil || first.Key() != "p/first" {
		t.Fatalf("expected first object before mid-stream error, got %v", first)
	}
	if sentinel := <-ch; sentinel != nil {
		t.Fatalf("expected nil sentinel on mid-stream error, got %v", sentinel)
	}
	if extra, ok := <-ch; ok {
		t.Fatalf("expected closed channel after sentinel, got %v", extra)
	}
}

func TestDMSListAllReturnsContextCancellationBeforeFirstPage(t *testing.T) {
	client := &mockDMSClient{
		scanFunc: func(ctx context.Context, _ string, _ dms.ScanOptions) (dms.ScanResult, error) {
			return dms.ScanResult{}, ctx.Err()
		},
	}
	store := newTestDMSStore(client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := store.ListAll(ctx, "p/", "", true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got channel=%v err=%v", ch, err)
	}
}

func TestDMSOptionalObjectStorageOperationsRemainUnsupported(t *testing.T) {
	store := newTestDMSStore(&mockDMSClient{})

	if err := store.Copy(context.Background(), "dst", "src"); !errors.Is(err, notSupported) {
		t.Fatalf("expected Copy notSupported, got %v", err)
	}
	if _, err := store.CreateMultipartUpload(context.Background(), "key"); !errors.Is(err, notSupported) {
		t.Fatalf("expected CreateMultipartUpload notSupported, got %v", err)
	}
	if _, err := store.UploadPart(context.Background(), "key", "upload", 1, []byte("part")); !errors.Is(err, notSupported) {
		t.Fatalf("expected UploadPart notSupported, got %v", err)
	}
	if _, err := store.UploadPartCopy(context.Background(), "key", "upload", 1, "src", 0, 1); !errors.Is(err, notSupported) {
		t.Fatalf("expected UploadPartCopy notSupported, got %v", err)
	}
	if err := store.CompleteUpload(context.Background(), "key", "upload", nil); !errors.Is(err, notSupported) {
		t.Fatalf("expected CompleteUpload notSupported, got %v", err)
	}
	if err := store.Restore(context.Background(), "key", 1); !errors.Is(err, notSupported) {
		t.Fatalf("expected Restore notSupported, got %v", err)
	}
	limits := store.Limits()
	if limits.IsSupportMultipartUpload || limits.IsSupportUploadPartCopy {
		t.Fatalf("unexpected optional support limits: %#v", limits)
	}
}

func newTestDMSStore(client *mockDMSClient) *dmsStore {
	return &dmsStore{
		client:  client,
		uri:     "dms://unit",
		writers: make(chan struct{}, 1),
	}
}

func readDMSObject(t *testing.T, store *dmsStore, key string, off, limit int64) string {
	t.Helper()
	reader, err := store.Get(context.Background(), key, off, limit)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read object failed: %v", err)
	}
	return string(data)
}

func keysOf(objects []Object) []string {
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.Key())
	}
	return keys
}

type failingReader struct {
	err error
}

func (r failingReader) Read([]byte) (int, error) {
	return 0, r.err
}

type mockDMSClient struct {
	mu sync.Mutex

	setKeys []string

	getCount int

	setFunc            func(context.Context, string, []byte) (dms.SetResult, error)
	setFromFunc        func(context.Context, string, io.Reader, uint64, dms.SetOptions) (dms.SetResult, error)
	getReaderFunc      func(context.Context, string, dms.GetOptions) (dms.ReadResult, bool, error)
	getFunc            func(context.Context, string) ([]byte, bool, error)
	getWithOptionsFunc func(context.Context, string, dms.GetOptions) (dms.GetResult, bool, error)
	delFunc            func(context.Context, string) (dms.DeleteResult, error)
	statFunc           func(context.Context, string) (dms.ObjectInfo, bool, error)
	scanFunc           func(context.Context, string, dms.ScanOptions) (dms.ScanResult, error)
	closeFunc          func() error
}

func (m *mockDMSClient) GetReader(ctx context.Context, key string, options dms.GetOptions) (dms.ReadResult, bool, error) {
	if m.getReaderFunc != nil {
		return m.getReaderFunc(ctx, key, options)
	}
	var data []byte
	var found bool
	var err error
	var version dms.ObjectVersion
	if options.Range == nil {
		data, found, err = m.Get(ctx, key)
	} else {
		var result dms.GetResult
		result, found, err = m.GetWithOptions(ctx, key, options)
		data, version = result.Bytes, result.Version
	}
	if err != nil || !found {
		return dms.ReadResult{}, found, err
	}
	return dms.ReadResult{Version: version, Len: uint64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, true, nil
}

func (m *mockDMSClient) SetFrom(ctx context.Context, key string, reader io.Reader, length uint64, options dms.SetOptions) (dms.SetResult, error) {
	if m.setFromFunc != nil {
		return m.setFromFunc(ctx, key, reader, length, options)
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(reader, data); err != nil {
		return dms.SetResult{}, err
	}
	return m.Set(ctx, key, data)
}

func (m *mockDMSClient) Set(ctx context.Context, key string, data []byte) (dms.SetResult, error) {
	m.mu.Lock()
	m.setKeys = append(m.setKeys, key)
	m.mu.Unlock()
	if m.setFunc != nil {
		return m.setFunc(ctx, key, data)
	}
	return dms.SetResult{Len: uint64(len(data))}, nil
}

func (m *mockDMSClient) Get(ctx context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	m.getCount++
	m.mu.Unlock()
	if m.getFunc != nil {
		return m.getFunc(ctx, key)
	}
	return nil, false, os.ErrNotExist
}

func (m *mockDMSClient) GetWithOptions(ctx context.Context, key string, options dms.GetOptions) (dms.GetResult, bool, error) {
	if m.getWithOptionsFunc != nil {
		return m.getWithOptionsFunc(ctx, key, options)
	}
	return dms.GetResult{}, false, os.ErrNotExist
}

func (m *mockDMSClient) Del(ctx context.Context, key string) (dms.DeleteResult, error) {
	if m.delFunc != nil {
		return m.delFunc(ctx, key)
	}
	return dms.DeleteResult{}, nil
}

func (m *mockDMSClient) Stat(ctx context.Context, key string) (dms.ObjectInfo, bool, error) {
	if m.statFunc != nil {
		return m.statFunc(ctx, key)
	}
	return dms.ObjectInfo{}, false, nil
}

func (m *mockDMSClient) Scan(ctx context.Context, prefix string, options dms.ScanOptions) (dms.ScanResult, error) {
	if m.scanFunc != nil {
		return m.scanFunc(ctx, prefix, options)
	}
	return dms.ScanResult{}, nil
}

func (m *mockDMSClient) Close() error {
	if m.closeFunc != nil {
		return m.closeFunc()
	}
	return nil
}

func (m *mockDMSClient) setCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.setKeys)
}

func (m *mockDMSClient) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getCount
}

func (m *mockDMSClient) String() string {
	return fmt.Sprintf("sets=%v gets=%d", m.setKeys, m.getCount)
}
