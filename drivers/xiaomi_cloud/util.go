package xiaomi_cloud

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

const (
	BaseURL   = "https://i.mi.com"
	APIPrefix = "/drive/v2/user"
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	Referer   = "https://i.mi.com/drive/h5"
	ChunkSize = 4 * 1024 * 1024 // 4MB 分块大小
)

// request 发送HTTP请求的通用方法
func (d *XiaomiCloud) request(url string, method string, callback base.ReqCallback, resp interface{}) ([]byte, error) {
	req := base.RestyClient.R()

	// 设置通用请求头
	req.SetHeaders(map[string]string{
		"User-Agent":      UserAgent,
		"Referer":         Referer,
		"Accept":          "*/*",
		"Accept-Language": "zh-CN,zh;q=0.9",
	})

	// 设置Cookie认证
	cookies := []*http.Cookie{
		{Name: "serviceToken", Value: d.ServiceToken},
		{Name: "userId", Value: d.UserID},
		{Name: "i.mi.com_slh", Value: d.SessionHash},
		{Name: "i.mi.com_ph", Value: d.UserHash},
		{Name: "uLocale", Value: "zh_CN"},
		{Name: "iplocale", Value: "zh_CN"},
	}
	req.SetCookies(cookies)

	// 添加时间戳参数(所有请求都需要)
	if !strings.Contains(url, "?") {
		url += "?"
	} else {
		url += "&"
	}
	url += fmt.Sprintf("ts=%d", time.Now().UnixMilli())

	// 执行回调设置额外参数
	if callback != nil {
		callback(req)
	}

	// 如果需要解析响应
	if resp != nil {
		req.SetResult(resp)
	}

	// 发送请求
	var res *resty.Response
	var err error
	switch method {
	case http.MethodGet:
		res, err = req.Get(url)
	case http.MethodPost:
		res, err = req.Post(url)
	case http.MethodPut:
		res, err = req.Put(url)
	case http.MethodDelete:
		res, err = req.Delete(url)
	default:
		return nil, fmt.Errorf("不支持的HTTP方法: %s", method)
	}

	if err != nil {
		return nil, err
	}

	// 检查HTTP状态码
	if res.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("HTTP错误: %d %s", res.StatusCode(), res.Status())
	}

	// 自动更新Cookie（从响应的Set-Cookie中提取）
	d.updateCookieFromResponse(res)

	body := res.Body()

	// 检查API响应码
	if resp != nil {
		// 尝试解析错误响应
		var errResp ErrorResp
		if err := json.Unmarshal(body, &errResp); err == nil {
			if errResp.Code != 0 && errResp.Result != "ok" {
				return body, fmt.Errorf("API错误: code=%d, msg=%s", errResp.Code, errResp.Description)
			}
		}
	}

	log.Debugf("[xiaomi_cloud] 请求成功: %s %s", method, url)
	return body, nil
}

// updateCookieFromResponse 从响应中提取并更新Cookie
// 小米云盘的安全机制：每次API响应都会通过Set-Cookie刷新serviceToken和session_hash
func (d *XiaomiCloud) updateCookieFromResponse(res *resty.Response) {
	setCookies := res.Cookies()
	if len(setCookies) == 0 {
		return
	}

	updated := false
	for _, cookie := range setCookies {
		switch cookie.Name {
		case "serviceToken":
			if cookie.Value != "" && cookie.Value != d.ServiceToken {
				oldMaxLen := 20
				if len(d.ServiceToken) < oldMaxLen {
					oldMaxLen = len(d.ServiceToken)
				}
				newMaxLen := 20
				if len(cookie.Value) < newMaxLen {
					newMaxLen = len(cookie.Value)
				}
				oldToken := d.ServiceToken[:oldMaxLen] + "..."
				newToken := cookie.Value[:newMaxLen] + "..."
				log.Debugf("[xiaomi_cloud] 服务令牌已更新: %s -> %s", oldToken, newToken)
				d.ServiceToken = cookie.Value
				updated = true
			}
		case "i.mi.com_slh":
			if cookie.Value != "" && cookie.Value != d.SessionHash {
				log.Debugf("[xiaomi_cloud] 会话哈希已更新: %s -> %s", d.SessionHash, cookie.Value)
				d.SessionHash = cookie.Value
				updated = true
			}
		case "userId":
			// userId一般不变，但也更新以防万一
			if cookie.Value != "" && cookie.Value != d.UserID {
				log.Debugf("[xiaomi_cloud] 用户ID已更新: %s -> %s", d.UserID, cookie.Value)
				d.UserID = cookie.Value
				updated = true
			}
		case "i.mi.com_ph":
			// user_hash一般不变，但也更新以防万一
			if cookie.Value != "" && cookie.Value != d.UserHash {
				log.Debugf("[xiaomi_cloud] 用户哈希已更新: %s -> %s", d.UserHash, cookie.Value)
				d.UserHash = cookie.Value
				updated = true
			}
		}
	}

	if updated {
		log.Infof("[xiaomi_cloud] Cookie已自动刷新")
	}
}

// getFiles 获取指定目录下的文件列表
func (d *XiaomiCloud) getFiles(parentID string) ([]File, error) {
	var allFiles []File
	pageNo := 1
	limit := 100

	for {
		var resp ListResp
		apiURL := fmt.Sprintf("%s%s/folders/children", BaseURL, APIPrefix)

		_, err := d.request(apiURL, http.MethodGet, func(req *resty.Request) {
			req.SetQueryParams(map[string]string{
				"parentId": parentID,
				"pageNo":   strconv.Itoa(pageNo),
				"limit":    strconv.Itoa(limit),
				"type":     "",
				"order":    d.OrderBy,
				"reverse":  strconv.FormatBool(d.OrderDirection == "DESC"),
			})
		}, &resp)

		if err != nil {
			return nil, err
		}

		if resp.Code != 0 {
			return nil, fmt.Errorf("获取文件列表失败: %s", resp.Description)
		}

		allFiles = append(allFiles, resp.Data.Records...)

		// 检查是否还有更多数据
		if !resp.Data.HasMore {
			break
		}

		pageNo++
	}

	return allFiles, nil
}

// calculateSHA1 计算文件的SHA1哈希值
func calculateSHA1(reader io.Reader) (string, error) {
	h := sha1.New()
	if _, err := io.Copy(h, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// calculateMD5 计算数据块的MD5哈希值
func calculateMD5(data []byte) string {
	h := md5.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// calculateBlockHash 计算文件块的SHA1和MD5
func calculateBlockHash(data []byte) (sha1Hash, md5Hash string) {
	// SHA1
	h1 := sha1.New()
	h1.Write(data)
	sha1Hash = hex.EncodeToString(h1.Sum(nil))

	// MD5
	h2 := md5.New()
	h2.Write(data)
	md5Hash = hex.EncodeToString(h2.Sum(nil))

	return
}

// waitForTask 轮询异步任务直到完成
func (d *XiaomiCloud) waitForTask(taskID string, maxWaitTime time.Duration) error {
	apiURL := fmt.Sprintf("%s%s/records/taskquery", BaseURL, APIPrefix)
	startTime := time.Now()
	interval := 200 * time.Millisecond // 初始轮询间隔

	for {
		// 检查超时
		if time.Since(startTime) > maxWaitTime {
			return fmt.Errorf("任务超时: taskId=%s", taskID)
		}

		var resp TaskQueryResp
		_, err := d.request(apiURL, http.MethodGet, func(req *resty.Request) {
			req.SetQueryParam("taskId", taskID)
		}, &resp)

		if err != nil {
			return err
		}

		if resp.Code != 0 {
			return fmt.Errorf("查询任务失败: %s", resp.Description)
		}

		switch resp.Data.Status {
		case "SUCCESS":
			return nil
		case "FAILED":
			return fmt.Errorf("任务失败: taskId=%s", taskID)
		case "PROCESSING":
			// 继续等待
			time.Sleep(interval)
			// 逐渐增加轮询间隔,最大1秒
			if interval < time.Second {
				interval += 100 * time.Millisecond
			}
		default:
			return fmt.Errorf("未知任务状态: %s", resp.Data.Status)
		}
	}
}

// buildFormData 构建表单数据(用于application/x-www-form-urlencoded)
func buildFormData(data map[string]interface{}) string {
	values := url.Values{}
	for key, value := range data {
		switch v := value.(type) {
		case string:
			values.Set(key, v)
		case int, int64:
			values.Set(key, fmt.Sprintf("%d", v))
		case bool:
			values.Set(key, strconv.FormatBool(v))
		default:
			// 对于复杂类型,序列化为JSON字符串
			jsonBytes, _ := json.Marshal(v)
			values.Set(key, string(jsonBytes))
		}
	}
	return values.Encode()
}

// isRetriableError 判断错误是否可重试
func isRetriableError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	// 网络错误、超时错误等可重试
	return strings.Contains(errMsg, "timeout") ||
		strings.Contains(errMsg, "connection") ||
		strings.Contains(errMsg, "temporary")
}

// retryRequest 带重试的请求
func (d *XiaomiCloud) retryRequest(maxRetries int, fn func() error) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetriableError(err) {
			return err
		}
		// 指数退避
		waitTime := time.Duration(1<<uint(i)) * time.Second
		log.Warnf("[xiaomi_cloud] 请求失败,等待%v后重试(第%d/%d次): %v", waitTime, i+1, maxRetries, err)
		time.Sleep(waitTime)
	}
	return lastErr
}

// normalizeError 规范化错误信息
func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	errMsg := err.Error()
	// 检查特定错误码
	if strings.Contains(errMsg, "code=401") || strings.Contains(errMsg, "未授权") {
		return errs.NewErr(errs.EmptyToken, "Cookie已过期或无效,请重新提取")
	}
	if strings.Contains(errMsg, "code=403") || strings.Contains(errMsg, "禁止访问") {
		return fmt.Errorf("访问被拒绝: %w", err)
	}
	if strings.Contains(errMsg, "code=404") || strings.Contains(errMsg, "不存在") {
		return errs.NewErr(errs.ObjectNotFound, "文件或文件夹不存在")
	}
	return err
}

// ParseCookieString 从完整的Cookie字符串中提取所需字段
// 支持两种格式：
// 1. 浏览器Cookie格式: "key1=value1; key2=value2; ..."
// 2. HTTP请求头格式: "key1=value1; key2=value2; ..."
func ParseCookieString(cookieStr string) (serviceToken, userId, sessionHash, userHash string, err error) {
	if cookieStr == "" {
		return "", "", "", "", fmt.Errorf("Cookie字符串为空")
	}

	// 解析Cookie键值对
	cookieMap := make(map[string]string)

	// 按分号分割Cookie项
	pairs := strings.Split(cookieStr, ";")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		// 按等号分割键值
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		cookieMap[key] = value
	}

	// 提取所需字段
	serviceToken = cookieMap["serviceToken"]
	userId = cookieMap["userId"]
	sessionHash = cookieMap["i.mi.com_slh"]
	userHash = cookieMap["i.mi.com_ph"]

	// 验证必需字段
	missingFields := []string{}
	if serviceToken == "" {
		missingFields = append(missingFields, "serviceToken")
	}
	if userId == "" {
		missingFields = append(missingFields, "userId")
	}
	if sessionHash == "" {
		missingFields = append(missingFields, "i.mi.com_slh")
	}
	if userHash == "" {
		missingFields = append(missingFields, "i.mi.com_ph")
	}

	if len(missingFields) > 0 {
		return "", "", "", "", fmt.Errorf("Cookie中缺少必需字段: %s", strings.Join(missingFields, ", "))
	}

	return serviceToken, userId, sessionHash, userHash, nil
}
