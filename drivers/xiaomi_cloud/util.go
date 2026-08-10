package xiaomi_cloud

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-resty/resty/v2"
)

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
