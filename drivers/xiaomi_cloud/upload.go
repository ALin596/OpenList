package xiaomi_cloud

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/errgroup"
	"github.com/avast/retry-go"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

const (
	chunkSize            int64 = 4 * 1024 * 1024
	defaultUploadThreads       = 4
	maxUploadThreads           = 32
	uploadAttempts             = 3
)

type uploadManifest struct {
	fileSHA1 string
	fileSize int64
	blocks   []blockInfo
}

type uploadSession struct {
	uploadID   string
	fileMeta   string
	nodeURLs   []string
	blockMetas []uploadBlockMeta
}

func (d *XiaomiCloud) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	cache, manifest, uploadProgress, err := prepareUpload(stream, up)
	if err != nil {
		return fmt.Errorf("准备上传文件失败: %w", err)
	}

	createResp, err := d.createUpload(ctx, dstDir, stream, manifest)
	if err != nil {
		return normalizeError(err)
	}
	if createResp.Data.Exists {
		log.Infof("[xiaomi_cloud] 文件秒传成功: %s (ID: %s)", stream.GetName(), createResp.Data.ID)
		up(100)
		return nil
	}

	session, err := newUploadSession(createResp, len(manifest.blocks))
	if err != nil {
		return err
	}
	log.Infof("[xiaomi_cloud] 开始并发分块上传: %s (%d 块, %d 线程)",
		stream.GetName(), len(manifest.blocks), min(len(manifest.blocks), normalizeUploadThreads(d.UploadThread)))

	commitMetas, err := d.uploadBlocks(ctx, cache, manifest, session, uploadProgress)
	if err != nil {
		return err
	}
	if err = d.commitUpload(ctx, dstDir.GetID(), session, commitMetas); err != nil {
		return err
	}

	log.Infof("[xiaomi_cloud] 文件上传成功: %s", stream.GetName())
	up(100)
	return nil
}

func prepareUpload(stream model.FileStreamer, up driver.UpdateProgress) (model.File, uploadManifest, driver.UpdateProgress, error) {
	hasher := newUploadHasher(chunkSize)
	// CacheFullAndWriter reports caching in the first half and remaps this callback
	// to the second half for the actual upload.
	uploadProgress := up
	cache, err := stream.CacheFullAndWriter(&uploadProgress, hasher)
	if err != nil {
		return nil, uploadManifest{}, uploadProgress, err
	}

	manifest, err := hasher.manifest(stream.GetSize())
	if err != nil {
		return nil, uploadManifest{}, uploadProgress, err
	}
	return cache, manifest, uploadProgress, nil
}

func (d *XiaomiCloud) createUpload(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, manifest uploadManifest) (*createUploadResponse, error) {
	now := time.Now().UnixMilli()
	uploadData, err := json.Marshal(uploadFileData{
		FileName:        stream.GetName(),
		Size:            manifest.fileSize,
		SHA1:            manifest.fileSHA1,
		MIMEType:        stream.GetMimetype(),
		Channel:         "WEB",
		ParentID:        dstDir.GetID(),
		LocalCreateTime: now,
		LocalModifyTime: now,
		ExifInfo:        map[string]interface{}{},
	})
	if err != nil {
		return nil, fmt.Errorf("序列化上传文件信息失败: %w", err)
	}
	kssData, err := json.Marshal(kssInfo{
		BlockInfos: manifest.blocks,
		SplitSize:  chunkSize,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化KSS分块信息失败: %w", err)
	}

	apiURL := fmt.Sprintf("%s%s/files/create", baseURL, apiPrefix)
	var resp createUploadResponse
	err = retryRequest(ctx, uploadAttempts, func() error {
		resp = createUploadResponse{}
		return d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
			req.SetFormData(map[string]string{
				"data":                    string(uploadData),
				"kss":                     string(kssData),
				"temp":                    "false",
				"isClientUploadThumbnail": "false",
				"serviceToken":            serviceToken,
			})
		}, &resp)
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func newUploadSession(resp *createUploadResponse, blockCount int) (uploadSession, error) {
	if resp.Data.UploadID == "" || resp.Data.KSS.FileMeta == "" {
		return uploadSession{}, fmt.Errorf("创建上传任务返回的KSS元数据不完整")
	}
	if len(resp.Data.KSS.NodeURLs) == 0 {
		return uploadSession{}, fmt.Errorf("创建上传任务未返回KSS节点地址")
	}
	if len(resp.Data.KSS.BlockMetas) != blockCount {
		return uploadSession{}, fmt.Errorf("分块数量不匹配: 预期 %d, 实际 %d", blockCount, len(resp.Data.KSS.BlockMetas))
	}
	for index, blockMeta := range resp.Data.KSS.BlockMetas {
		if blockMeta.BlockMeta == "" {
			return uploadSession{}, fmt.Errorf("分块 %d 未返回block_meta", index+1)
		}
	}
	return uploadSession{
		uploadID:   resp.Data.UploadID,
		fileMeta:   resp.Data.KSS.FileMeta,
		nodeURLs:   resp.Data.KSS.NodeURLs,
		blockMetas: resp.Data.KSS.BlockMetas,
	}, nil
}

func (d *XiaomiCloud) uploadBlocks(
	ctx context.Context,
	cache model.File,
	manifest uploadManifest,
	session uploadSession,
	up driver.UpdateProgress,
) ([]commitMeta, error) {
	blockCount := len(manifest.blocks)
	if blockCount == 0 {
		return []commitMeta{}, nil
	}

	threadCount := min(blockCount, normalizeUploadThreads(d.UploadThread))
	group, uploadCtx := errgroup.NewGroupWithContext(ctx, threadCount,
		retry.Attempts(uploadAttempts),
		retry.Delay(time.Second),
		retry.DelayType(retry.BackOffDelay),
		retry.RetryIf(isRetriableError),
		retry.LastErrorOnly(true),
	)
	commitMetas := make([]commitMeta, blockCount)
	progress := newCompletedBlockProgress(manifest.fileSize, up)

	for index := range blockCount {
		if uploadCtx.Err() != nil {
			break
		}
		index := index
		blockInfo := manifest.blocks[index]
		blockMeta := session.blockMetas[index].BlockMeta
		uploadURL := buildKSSUploadURL(session.nodeURLs[index%len(session.nodeURLs)], session.fileMeta, blockMeta)
		offset := int64(index) * chunkSize

		group.Go(func(ctx context.Context) error {
			blockResp, err := uploadBlock(ctx, cache, offset, blockInfo.Size, uploadURL)
			if err != nil {
				return fmt.Errorf("上传分块 %d 失败: %w", index+1, err)
			}
			commitMetas[index] = commitMeta{CommitMeta: blockResp.CommitMeta}
			progress.add(blockInfo.Size)
			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}
	return commitMetas, nil
}

func uploadBlock(ctx context.Context, cache model.File, offset, size int64, uploadURL string) (*uploadBlockResponse, error) {
	reader := io.NewSectionReader(cache, offset, size)
	requestURL, err := addTimestamp(uploadURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, driver.NewLimitedUploadFile(ctx, reader))
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", strconv.FormatInt(size, 10))

	httpResp, err := base.RestyClient.GetClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, httpResp.Body)
		return nil, &httpStatusError{code: httpResp.StatusCode, status: httpResp.Status}
	}
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	var resp uploadBlockResponse
	if err = json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析分块上传响应失败: %w", err)
	}
	if resp.Stat != "BLOCK_COMPLETED" {
		return nil, fmt.Errorf("上传状态异常: %s", resp.Stat)
	}
	if resp.CommitMeta == "" {
		return nil, fmt.Errorf("响应缺少commit_meta")
	}
	return &resp, nil
}

func (d *XiaomiCloud) commitUpload(ctx context.Context, parentID string, session uploadSession, commitMetas []commitMeta) error {
	commitData, err := json.Marshal(commitMetas)
	if err != nil {
		return fmt.Errorf("序列化分块提交信息失败: %w", err)
	}

	commitURL := fmt.Sprintf("%s%s/files/commit", baseURL, apiPrefix)
	err = retryRequest(ctx, uploadAttempts, func() error {
		return d.request(ctx, commitURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
			req.SetFormData(map[string]string{
				"uploadId":                session.uploadID,
				"file_meta":               session.fileMeta,
				"commit_metas":            string(commitData),
				"parentId":                parentID,
				"temp":                    "false",
				"isClientUploadThumbnail": "false",
				"serviceToken":            serviceToken,
			})
		}, nil)
	})
	if err != nil {
		return fmt.Errorf("提交上传失败: %w", err)
	}
	return nil
}

func normalizeUploadThreads(threadCount int) int {
	if threadCount < 1 {
		return defaultUploadThreads
	}
	return min(threadCount, maxUploadThreads)
}

func buildKSSUploadURL(nodeURL, fileMeta, blockMeta string) string {
	query := url.Values{}
	query.Set("chunk_pos", "0")
	query.Set("file_meta", fileMeta)
	query.Set("block_meta", blockMeta)
	return fmt.Sprintf("%s/upload_block_chunk?%s", strings.TrimRight(nodeURL, "/"), query.Encode())
}

func addTimestamp(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("解析上传地址失败: %w", err)
	}
	query := parsed.Query()
	query.Set("ts", strconv.FormatInt(time.Now().UnixMilli(), 10))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

type uploadHasher struct {
	blockSize int64
	fileHash  hash.Hash
	blockSHA1 hash.Hash
	blockMD5  hash.Hash
	blockLen  int64
	total     int64
	blocks    []blockInfo
}

func newUploadHasher(blockSize int64) *uploadHasher {
	return &uploadHasher{
		blockSize: blockSize,
		fileHash:  sha1.New(),
		blockSHA1: sha1.New(),
		blockMD5:  md5.New(),
	}
}

func (h *uploadHasher) Write(p []byte) (int, error) {
	written := len(p)
	_, _ = h.fileHash.Write(p)
	h.total += int64(written)

	for len(p) > 0 {
		remaining := h.blockSize - h.blockLen
		partSize := min(int64(len(p)), remaining)
		part := p[:int(partSize)]
		_, _ = h.blockSHA1.Write(part)
		_, _ = h.blockMD5.Write(part)
		h.blockLen += partSize
		p = p[int(partSize):]
		if h.blockLen == h.blockSize {
			h.finishBlock()
		}
	}
	return written, nil
}

func (h *uploadHasher) manifest(expectedSize int64) (uploadManifest, error) {
	if h.blockLen > 0 {
		h.finishBlock()
	}
	if expectedSize < 0 {
		expectedSize = h.total
	}
	if h.total != expectedSize {
		return uploadManifest{}, fmt.Errorf("文件大小不匹配: 预期 %d, 实际 %d", expectedSize, h.total)
	}
	return uploadManifest{
		fileSHA1: hex.EncodeToString(h.fileHash.Sum(nil)),
		fileSize: h.total,
		blocks:   h.blocks,
	}, nil
}

func (h *uploadHasher) finishBlock() {
	h.blocks = append(h.blocks, blockInfo{
		SHA1: hex.EncodeToString(h.blockSHA1.Sum(nil)),
		MD5:  hex.EncodeToString(h.blockMD5.Sum(nil)),
		Size: h.blockLen,
	})
	h.blockSHA1.Reset()
	h.blockMD5.Reset()
	h.blockLen = 0
}

type completedBlockProgress struct {
	mu    sync.Mutex
	done  int64
	total int64
	up    driver.UpdateProgress
}

func newCompletedBlockProgress(total int64, up driver.UpdateProgress) *completedBlockProgress {
	return &completedBlockProgress{total: total, up: up}
}

func (p *completedBlockProgress) add(size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += size
	if p.total > 0 {
		p.up(min(95, float64(p.done)*95/float64(p.total)))
	}
}
