package xiaomi_cloud

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
)

const recentFileTTL = time.Minute

type recentFile struct {
	file      file
	expiresAt time.Time
}

func (d *XiaomiCloud) getFiles(ctx context.Context, parentID string) ([]file, error) {
	var allFiles []file
	for pageNo := 1; ; pageNo++ {
		var resp listResponse
		apiURL := fmt.Sprintf("%s%s/folders/children", baseURL, apiPrefix)
		err := d.request(ctx, apiURL, http.MethodGet, func(req *resty.Request, _ string) {
			req.SetQueryParams(map[string]string{
				"parentId": parentID,
				"pageNo":   strconv.Itoa(pageNo),
				"limit":    "100",
				"type":     "",
				"order":    d.OrderBy,
				"reverse":  strconv.FormatBool(d.OrderDirection == "DESC"),
			})
		}, &resp)
		if err != nil {
			return nil, err
		}
		allFiles = append(allFiles, resp.Data.Records...)
		if !resp.Data.HasMore {
			return allFiles, nil
		}
	}
}

func (d *XiaomiCloud) getFile(ctx context.Context, fileID string) (file, error) {
	apiURL := fmt.Sprintf("%s%s/files/%s", baseURL, apiPrefix, fileID)
	var resp fileInfoResponse
	if err := d.request(ctx, apiURL, http.MethodGet, nil, &resp); err != nil {
		return file{}, err
	}
	return resp.Data, nil
}

func (d *XiaomiCloud) rememberRecentFile(parentID string, uploaded file) {
	d.recentFileMu.Lock()
	defer d.recentFileMu.Unlock()
	d.pruneExpiredRecentFilesLocked(time.Now())
	if d.recentFiles == nil {
		d.recentFiles = make(map[string]map[string]recentFile)
	}
	if d.recentFiles[parentID] == nil {
		d.recentFiles[parentID] = make(map[string]recentFile)
	}
	d.recentFiles[parentID][uploaded.Name] = recentFile{
		file:      uploaded,
		expiresAt: time.Now().Add(recentFileTTL),
	}
}

func (d *XiaomiCloud) mergeRecentFiles(parentID string, files []file) []file {
	d.recentFileMu.Lock()
	defer d.recentFileMu.Unlock()
	now := time.Now()
	d.pruneExpiredRecentFilesLocked(now)
	recent := d.recentFiles[parentID]
	if len(recent) == 0 {
		return files
	}

	merged := append([]file(nil), files...)
	byName := make(map[string]int, len(merged))
	for index, current := range merged {
		byName[current.Name] = index
	}
	for name, pending := range recent {
		if index, ok := byName[name]; ok {
			if merged[index].ID == pending.file.ID {
				delete(recent, name)
			} else {
				// Prefer the committed object over an older same-name directory entry.
				merged[index] = pending.file
			}
			continue
		}
		byName[name] = len(merged)
		merged = append(merged, pending.file)
	}
	if len(recent) == 0 {
		delete(d.recentFiles, parentID)
	}
	return merged
}

func (d *XiaomiCloud) pruneExpiredRecentFilesLocked(now time.Time) {
	for parentID, files := range d.recentFiles {
		for name, recent := range files {
			if now.After(recent.expiresAt) {
				delete(files, name)
			}
		}
		if len(files) == 0 {
			delete(d.recentFiles, parentID)
		}
	}
}

func (d *XiaomiCloud) forgetRecentFile(fileID string) {
	d.recentFileMu.Lock()
	defer d.recentFileMu.Unlock()
	for parentID, files := range d.recentFiles {
		for name, recent := range files {
			if recent.file.ID == fileID {
				delete(files, name)
			}
		}
		if len(files) == 0 {
			delete(d.recentFiles, parentID)
		}
	}
}

func (d *XiaomiCloud) renameRecentFile(fileID, newName string) {
	d.recentFileMu.Lock()
	defer d.recentFileMu.Unlock()
	for _, files := range d.recentFiles {
		for name, recent := range files {
			if recent.file.ID != fileID {
				continue
			}
			delete(files, name)
			recent.file.Name = newName
			files[newName] = recent
			return
		}
	}
}

func (d *XiaomiCloud) moveRecentFile(fileID, parentID string) {
	d.recentFileMu.Lock()
	defer d.recentFileMu.Unlock()
	for oldParentID, files := range d.recentFiles {
		for name, recent := range files {
			if recent.file.ID != fileID {
				continue
			}
			delete(files, name)
			if len(files) == 0 {
				delete(d.recentFiles, oldParentID)
			}
			if d.recentFiles[parentID] == nil {
				d.recentFiles[parentID] = make(map[string]recentFile)
			}
			recent.file.ParentID = flexibleID(parentID)
			d.recentFiles[parentID][recent.file.Name] = recent
			return
		}
	}
}

func validateUploadedFile(uploaded file, fileID, parentID, name string, size int64, sha1 string) error {
	switch {
	case uploaded.ID != fileID:
		return fmt.Errorf("文件ID不匹配: %s != %s", uploaded.ID, fileID)
	case string(uploaded.ParentID) != parentID:
		return fmt.Errorf("父目录ID不匹配: %s != %s", uploaded.ParentID, parentID)
	case uploaded.Name != name:
		return fmt.Errorf("文件名不匹配: %s != %s", uploaded.Name, name)
	case uploaded.Size != size:
		return fmt.Errorf("文件大小不匹配: %d != %d", uploaded.Size, size)
	case !strings.EqualFold(uploaded.SHA1, sha1):
		return fmt.Errorf("文件SHA1不匹配")
	default:
		return nil
	}
}

func (d *XiaomiCloud) waitForTask(ctx context.Context, taskID string, maxWaitTime time.Duration) error {
	taskCtx, cancel := context.WithTimeout(ctx, maxWaitTime)
	defer cancel()

	apiURL := fmt.Sprintf("%s%s/records/taskquery", baseURL, apiPrefix)
	interval := 200 * time.Millisecond
	for {
		var resp taskQueryResponse
		err := d.request(taskCtx, apiURL, http.MethodGet, func(req *resty.Request, _ string) {
			req.SetQueryParam("taskId", taskID)
		}, &resp)
		if err != nil {
			if taskCtx.Err() != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("任务超时: taskId=%s", taskID)
			}
			return err
		}
		done, statusErr := evaluateTaskStatus(resp.Data.Status, taskID)
		if statusErr != nil || done {
			return statusErr
		}

		timer := time.NewTimer(interval)
		select {
		case <-taskCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("任务超时: taskId=%s", taskID)
		case <-timer.C:
		}
		interval = min(interval+100*time.Millisecond, time.Second)
	}
}

func evaluateTaskStatus(status, taskID string) (bool, error) {
	switch status {
	case "SUCCESS":
		return true, nil
	case "RUNNING", "PROCESSING":
		return false, nil
	case "FAILED":
		return false, fmt.Errorf("任务失败: taskId=%s", taskID)
	default:
		return false, fmt.Errorf("未知任务状态: %s", status)
	}
}
