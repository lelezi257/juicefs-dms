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
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strconv"

	dms "github.com/lelezi257/dms/sdk/go"
)

const dmsObjectLimit = 8 << 20

// dmsObjectClient 只列适配器实际消费的原生 SDK 接口，便于隔离验证范围与错误转换。
// 这里不定义第二套 wire 协议，也不缓存 value 或 Head 的结果。
type dmsObjectClient interface {
	Set(context.Context, string, []byte) (dms.SetResult, error)
	Get(context.Context, string) ([]byte, bool, error)
	GetWithOptions(context.Context, string, dms.GetOptions) (dms.GetResult, bool, error)
	Del(context.Context, string) (dms.DeleteResult, error)
	Stat(context.Context, string) (dms.ObjectInfo, bool, error)
	Scan(context.Context, string, dms.ScanOptions) (dms.ScanResult, error)
	Close() error
}

type dmsStore struct {
	DefaultObjectStorage
	client dmsObjectClient
	uri    string
	// 限制 Reader 物化的并发内存；不在这里改变上游 Chunk 的分片方式。
	writers chan struct{}
}

func (s *dmsStore) String() string { return s.uri + "/" }

func (s *dmsStore) Shutdown() {
	if err := s.client.Close(); err != nil {
		logger.Warnf("close DMS object backend: %s", err)
	}
}

func (s *dmsStore) Put(ctx context.Context, key string, in io.Reader, getters ...AttrGetter) error {
	select {
	case s.writers <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.writers }()
	// +1 区分正好到上限和被截断；Reader 错误/超限时不能提交部分对象。
	data, err := io.ReadAll(io.LimitReader(dmsContextReader{ctx, in}, dmsObjectLimit+1))
	if err != nil {
		return err
	}
	if len(data) > dmsObjectLimit {
		return fmt.Errorf("DMS object exceeds %d byte integration limit", dmsObjectLimit)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = s.client.Set(ctx, key, data)
	return err
}

// 任意 io.Reader 无法被强行中断；每次 Read 前检查取消，底层阻塞 Reader 仍须自行支持取消。
type dmsContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r dmsContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

func (s *dmsStore) Get(ctx context.Context, key string, off, limit int64, getters ...AttrGetter) (io.ReadCloser, error) {
	if off < 0 || (off > 0 && limit < 0) {
		return nil, fmt.Errorf("invalid DMS object range: offset=%d limit=%d", off, limit)
	}
	var data []byte
	var found bool
	var err error
	if off == 0 && limit <= 0 {
		data, found, err = s.client.Get(ctx, key)
	} else {
		// SDK 的 Range 是严格的 offset+length；对象接口允许末端裁剪。
		// Stat 后固定版本，避免并发覆盖把旧长度与新 bytes 拼到一起。
		info, exists, statErr := s.client.Stat(ctx, key)
		if statErr != nil {
			return nil, statErr
		}
		if !exists {
			return nil, os.ErrNotExist
		}
		start := uint64(off)
		if start > info.Len {
			start = info.Len
		}
		length := info.Len - start
		if limit > 0 && uint64(limit) < length {
			length = uint64(limit)
		}
		result, exists, getErr := s.client.GetWithOptions(ctx, key, dms.GetOptions{
			Version: dms.ReadExact(info.Version), Range: &dms.ByteRange{Offset: start, Len: length},
		})
		data, found, err = result.Bytes, exists, getErr
	}
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, os.ErrNotExist
	}
	// SDK 已返回 owned bytes；Close 无需再次释放共享内存，更不改变 Block 寿命。
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *dmsStore) Delete(ctx context.Context, key string, getters ...AttrGetter) error {
	_, err := s.client.Del(ctx, key)
	return err
}

func dmsObject(info dms.ObjectInfo) (Object, error) {
	if info.Len > math.MaxInt64 {
		return nil, fmt.Errorf("DMS object size cannot be represented by ObjectStorage")
	}
	return &obj{key: info.Key, size: int64(info.Len), mtime: info.ModifiedTime}, nil
}

func (s *dmsStore) Head(ctx context.Context, key string) (Object, error) {
	info, found, err := s.client.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, os.ErrNotExist
	}
	return dmsObject(info)
}

func (s *dmsStore) List(ctx context.Context, prefix, startAfter, token, delimiter string, limit int64, followLink bool) ([]Object, bool, string, error) {
	if delimiter != "" {
		return nil, false, "", notSupported
	}
	if limit <= 0 {
		return nil, false, "", fmt.Errorf("DMS list limit must be positive")
	}
	if token != "" && startAfter != "" {
		return nil, false, "", fmt.Errorf("DMS list token and startAfter are mutually exclusive")
	}
	if limit > 1000 {
		limit = 1000
	}
	options := dms.ScanOptions{Limit: uint32(limit), Cursor: token}
	if startAfter != "" {
		options.StartAfter = &startAfter
	}
	page, err := s.client.Scan(ctx, prefix, options)
	if err != nil {
		return nil, false, "", err
	}
	objects := make([]Object, 0, len(page.Items))
	for _, info := range page.Items {
		object, err := dmsObject(info)
		if err != nil {
			return nil, false, "", err
		}
		objects = append(objects, object)
	}
	return objects, page.NextCursor != "", page.NextCursor, nil
}

func (s *dmsStore) ListAll(ctx context.Context, prefix, marker string, followLink bool) (<-chan Object, error) {
	// 第一次失败直接返回；中途失败遵循上游 nil 哨兵协议，不能伪装成正常 EOF。
	page, more, token, err := s.List(ctx, prefix, marker, "", "", 1000, followLink)
	if err != nil {
		return nil, err
	}
	out := make(chan Object, 1000)
	go func() {
		defer close(out)
		for {
			for _, object := range page {
				select {
				case out <- object:
				case <-ctx.Done():
					return
				}
			}
			if !more {
				return
			}
			page, more, token, err = s.List(ctx, prefix, "", token, "", 1000, followLink)
			if err != nil {
				logger.Warnf("DMS object scan failed: %s", err)
				select {
				case out <- nil:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return out, nil
}

func newDMS(bucket, accessKey, secretKey, token string) (ObjectStorage, error) {
	u, err := url.Parse(bucket)
	if err != nil || u.Scheme != "dms" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("DMS bucket must be dms://<cluster-alias>")
	}
	if accessKey != "" || secretKey != "" || token != "" {
		return nil, fmt.Errorf("DMS object backend does not accept object-store credentials")
	}
	// 卷中保存稳定 alias，各挂载进程映射到自己的本地或远端 Node。
	var endpoints map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("DMS_JUICEFS_ENDPOINTS")), &endpoints); err != nil {
		return nil, fmt.Errorf("DMS_JUICEFS_ENDPOINTS must be an alias-to-endpoint JSON object: %w", err)
	}
	endpoint := endpoints[u.Host]
	if endpoint == "" {
		return nil, fmt.Errorf("no DMS endpoint configured for alias %q", u.Host)
	}
	parallel := 4
	if value := os.Getenv("DMS_JUICEFS_MAX_INFLIGHT_PUTS"); value != "" {
		parallel, err = strconv.Atoi(value)
		if err != nil || parallel < 1 || parallel > 64 {
			return nil, fmt.Errorf("DMS_JUICEFS_MAX_INFLIGHT_PUTS must be in 1..64")
		}
	}
	client, err := dms.Connect(context.Background(), endpoint, dms.ClientOptions{})
	if err != nil {
		return nil, err
	}
	return &dmsStore{client: client, uri: "dms://" + u.Host, writers: make(chan struct{}, parallel)}, nil
}

func init() { Register("dms", newDMS) }
