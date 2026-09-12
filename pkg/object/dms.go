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
	"strings"

	dms "github.com/lelezi257/dms/sdk/go"
)

const dmsObjectLimit = 8 << 20

// dmsObjectClient 只列适配器实际消费的原生 SDK 接口，便于隔离验证范围与错误转换。
// 这里不定义第二套 wire 协议，也不缓存 value 或 Head 的结果。
type dmsObjectClient interface {
	SetFrom(context.Context, string, io.Reader, uint64, dms.SetOptions) (dms.SetResult, error)
	GetReader(context.Context, string, dms.GetOptions) (dms.ReadResult, bool, error)
	Del(context.Context, string) (dms.DeleteResult, error)
	Stat(context.Context, string) (dms.ObjectInfo, bool, error)
	Scan(context.Context, string, dms.ScanOptions) (dms.ScanResult, error)
	Close() error
}

type dmsStore struct {
	DefaultObjectStorage
	client dmsObjectClient
	uri    string
	// 限制在途写入及未知长度 Reader 的物化；不改变上游 Chunk 分片。
	writers chan struct{}
}

func (s *dmsStore) String() string { return s.uri + "/" }

// DMS 无需预建 bucket；该接口是 format 的容器准备步骤，不是创建用户对象。
func (s *dmsStore) Create(ctx context.Context) error { return ctx.Err() }

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
	if err := ctx.Err(); err != nil {
		return err
	}
	length, known, err := dmsReaderLength(in)
	if err != nil {
		return err
	}
	if !known {
		// 与通用对象 Reader 一样，未知长度且不可回放的源需要先确定完整输入；
		// 只在该后备分支物化。正常 Chunk 的 bytes.Reader 和文件源不会进入这里。
		// +1 用于区分上限与截断；读失败/超限绝不发布部分对象。
		data, readErr := io.ReadAll(io.LimitReader(dmsContextReader{ctx, in}, dmsObjectLimit+1))
		if readErr != nil {
			return readErr
		}
		length = int64(len(data))
		in = bytes.NewReader(data)
	}
	if length < 0 || length > dmsObjectLimit {
		return fmt.Errorf("DMS object exceeds %d byte integration limit", dmsObjectLimit)
	}
	// SDK 决定 SHM 直接填充或 TCP 协议缓冲；Adapter 不复制普通 Reader。
	_, err = s.client.SetFrom(ctx, key, dmsContextReader{ctx, in}, uint64(length), dms.SetOptions{})
	return err
}

// 计算剩余输入而非源的总长度。Seek 仅探测边界，调用 SetFrom 前恢复当前位置。
func dmsReaderLength(in io.Reader) (int64, bool, error) {
	if sized, ok := in.(interface{ Len() int }); ok {
		return int64(sized.Len()), true, nil
	}
	if seeker, ok := in.(io.Seeker); ok {
		start, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, false, nil
		}
		end, endErr := seeker.Seek(0, io.SeekEnd)
		if _, err := seeker.Seek(start, io.SeekStart); err != nil {
			return 0, false, err
		}
		if endErr != nil {
			return 0, false, nil
		}
		if end < start {
			return 0, false, fmt.Errorf("DMS input position is past end")
		}
		return end - start, true, nil
	}
	return 0, false, nil
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
	options := dms.GetOptions{}
	if off != 0 || limit > 0 {
		// 显式请求 Node 在本次选中版本上裁剪，不先 Stat，也不改变 SDK 默认严格范围。
		length := uint64(math.MaxUint64) - uint64(off)
		if limit > 0 {
			length = uint64(limit)
		}
		options.Range = &dms.ByteRange{Offset: uint64(off), Len: length}
		options.ClampRange = true
	}
	result, found, err := s.client.GetReader(ctx, key, options)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, os.ErrNotExist
	}
	// Reader 持有同一次版本和读保护；上游 Read(p) 直接提供 Chunk 目标内存。
	// Close 原样交给 SDK，Adapter 不维护第二套票据、mmap 或生命周期。
	return result.Body, nil
}

func (s *dmsStore) Delete(ctx context.Context, key string, getters ...AttrGetter) error {
	_, err := s.client.Del(ctx, key)
	return err
}

func dmsObject(info dms.ObjectInfo) (Object, error) {
	if info.Len > math.MaxInt64 {
		return nil, fmt.Errorf("DMS object size cannot be represented by ObjectStorage")
	}
	// DMS IsPrefix only marks SDK-synthesized delimiter groups. JuiceFS object
	// adapters still expose slash-suffixed real keys as directories, matching S3.
	return &obj{key: info.Key, size: int64(info.Len), mtime: info.ModifiedTime, isDir: info.IsPrefix || strings.HasSuffix(info.Key, "/")}, nil
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
	if limit <= 0 {
		return nil, false, "", fmt.Errorf("DMS list limit must be positive")
	}
	if limit > 1000 {
		limit = 1000
	}
	options := dms.ScanOptions{Limit: uint32(limit), Cursor: token, Delimiter: delimiter}
	// 续页 token 已携带位置；忽略调用方保留的初始 marker，避免追加额外 Scan。
	if startAfter != "" && token == "" {
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
