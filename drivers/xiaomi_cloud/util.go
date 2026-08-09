package xiaomi_cloud

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/publicsuffix"
)

const (
	BaseURL          = "https://i.mi.com"
	AccountBaseURL   = "https://account.xiaomi.com"
	APIPrefix        = "/drive/v2/user"
	UserAgent        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	Referer          = "https://i.mi.com/drive/h5"
	ChunkSize        = 4 * 1024 * 1024 // 4MB 分块大小
	maxAuthRedirects = 10
)

type requestCallback func(req *resty.Request, serviceToken string)

type sessionCookies struct {
	serviceToken string
	userID       string
	sessionHash  string
	userHash     string
}

var newXiaomiAuthClient = base.NewRestyClient

// request 发送HTTP请求的通用方法
func (d *XiaomiCloud) request(url string, method string, callback requestCallback, resp interface{}) ([]byte, error) {
	if isXiaomiAPIURL(url) && d.sessionSnapshot().serviceToken == "" {
		if err := d.ensureServiceToken(""); err != nil {
			return nil, err
		}
	}

	res, usedSession, err := d.doRequest(url, method, callback)
	if err != nil {
		return nil, err
	}

	if res.StatusCode() == http.StatusUnauthorized && isXiaomiAPIURL(url) {
		if err = d.ensureServiceToken(usedSession.serviceToken); err != nil {
			return nil, err
		}
		res, _, err = d.doRequest(url, method, callback)
		if err != nil {
			return nil, err
		}
	}

	if res.StatusCode() != http.StatusOK {
		if res.StatusCode() == http.StatusUnauthorized {
			return nil, errs.NewErr(errs.EmptyToken, "account_cookie 无法续签有效的 serviceToken")
		}
		return nil, fmt.Errorf("HTTP错误: %d %s", res.StatusCode(), res.Status())
	}

	d.updateSessionFromResponse(res)
	body := res.Body()

	// 小米KSS上传接口返回JSON响应体，但不设置Content-Type。
	// 不依赖Resty的Content-Type检测，统一显式解析响应。
	if resp != nil {
		if err := json.Unmarshal(body, resp); err != nil {
			return body, fmt.Errorf("解析响应失败: %w", err)
		}

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

func (d *XiaomiCloud) doRequest(url string, method string, callback requestCallback) (*resty.Response, sessionCookies, error) {
	req := base.RestyClient.R()
	session := d.sessionSnapshot()

	req.SetHeaders(map[string]string{
		"User-Agent":      UserAgent,
		"Referer":         Referer,
		"Accept":          "*/*",
		"Accept-Language": "zh-CN,zh;q=0.9",
	})

	if isXiaomiAPIURL(url) {
		cookies := session.httpCookies()
		cookies = append(cookies,
			&http.Cookie{Name: "uLocale", Value: "zh_CN"},
			&http.Cookie{Name: "iplocale", Value: "zh_CN"},
		)
		req.SetCookies(cookies)
	}

	if !strings.Contains(url, "?") {
		url += "?"
	} else {
		url += "&"
	}
	url += fmt.Sprintf("ts=%d", time.Now().UnixMilli())

	if callback != nil {
		callback(req, session.serviceToken)
	}

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
		return nil, session, fmt.Errorf("不支持的HTTP方法: %s", method)
	}

	return res, session, err
}

func (d *XiaomiCloud) sessionSnapshot() sessionCookies {
	d.sessionMu.RLock()
	defer d.sessionMu.RUnlock()
	return sessionCookies{
		serviceToken: d.serviceToken,
		userID:       d.userID,
		sessionHash:  d.sessionHash,
		userHash:     d.userHash,
	}
}

func (d *XiaomiCloud) setSession(session sessionCookies) {
	d.sessionMu.Lock()
	d.serviceToken = session.serviceToken
	d.userID = session.userID
	d.sessionHash = session.sessionHash
	d.userHash = session.userHash
	d.sessionMu.Unlock()
}

func (d *XiaomiCloud) updateSessionFromResponse(res *resty.Response) {
	session := d.sessionSnapshot()
	updated := session.apply(res.Cookies())
	if updated {
		d.setSession(session)
	}
}

func (d *XiaomiCloud) ensureServiceToken(staleToken string) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	currentToken := d.sessionSnapshot().serviceToken
	if currentToken != "" && (staleToken == "" || currentToken != staleToken) {
		return nil
	}

	if err := d.refreshServiceToken(); err != nil {
		return errs.NewErr(errs.EmptyToken, "account_cookie 已失效或需要完成小米账号安全验证: %v", err)
	}
	return nil
}

func (d *XiaomiCloud) refreshServiceToken() error {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return fmt.Errorf("创建 Cookie Jar 失败: %w", err)
	}

	accountURL, _ := url.Parse(AccountBaseURL + "/pass/serviceLogin")
	cloudURL, _ := url.Parse(BaseURL + "/")
	accountCookies := parseCookieHeader(d.AccountCookie)
	if !containsCookie(accountCookies, "passToken") {
		return fmt.Errorf("account_cookie 中缺少 passToken")
	}
	jar.SetCookies(accountURL, accountCookies)
	jar.SetCookies(cloudURL, d.sessionSnapshot().httpCookies())

	client := newXiaomiAuthClient().SetCookieJar(jar).SetRedirectPolicy(
		resty.RedirectPolicyFunc(func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxAuthRedirects {
				return fmt.Errorf("登录跳转次数超过限制")
			}
			switch strings.ToLower(req.URL.Hostname()) {
			case "account.xiaomi.com", "i.mi.com":
				return nil
			default:
				return fmt.Errorf("拒绝登录跳转到非小米域名")
			}
		}),
	)

	loginResp, err := client.R().
		SetHeaders(map[string]string{
			"User-Agent":      UserAgent,
			"Referer":         Referer,
			"Accept":          "application/json, text/plain, */*",
			"Accept-Language": "zh-CN,zh;q=0.9",
		}).
		SetQueryParams(map[string]string{
			"ts":       strconv.FormatInt(time.Now().UnixMilli(), 10),
			"followUp": Referer,
			"_locale":  "zh_CN",
		}).
		Get(BaseURL + "/api/user/login")
	if err != nil {
		return fmt.Errorf("获取登录地址失败: %w", err)
	}
	if loginResp.StatusCode() != http.StatusOK {
		return fmt.Errorf("获取登录地址失败: HTTP %d", loginResp.StatusCode())
	}

	var loginResult struct {
		Code int `json:"code"`
		Data struct {
			LoginURL string `json:"loginUrl"`
		} `json:"data"`
	}
	if err = json.Unmarshal(loginResp.Body(), &loginResult); err != nil {
		return fmt.Errorf("解析登录地址失败: %w", err)
	}
	if loginResult.Code != 0 || loginResult.Data.LoginURL == "" {
		return fmt.Errorf("小米云盘未返回登录地址")
	}

	serviceLoginURL, err := url.Parse(loginResult.Data.LoginURL)
	if err != nil || serviceLoginURL.Scheme != "https" || !strings.EqualFold(serviceLoginURL.Hostname(), "account.xiaomi.com") {
		return fmt.Errorf("小米云盘返回了无效的登录地址")
	}

	serviceResp, err := client.R().
		SetHeaders(map[string]string{
			"User-Agent":      UserAgent,
			"Referer":         Referer,
			"Accept-Language": "zh-CN,zh;q=0.9",
		}).
		Get(serviceLoginURL.String())
	if err != nil {
		return fmt.Errorf("执行小米账号登录失败: %w", err)
	}
	if serviceResp.StatusCode() >= http.StatusBadRequest {
		return fmt.Errorf("执行小米账号登录失败: HTTP %d", serviceResp.StatusCode())
	}

	newSession := sessionCookiesFromHTTP(jar.Cookies(cloudURL))
	if !newSession.complete() {
		return fmt.Errorf("账号登录未签发完整的云盘会话")
	}

	newAccountCookie := cookieHeader(jar.Cookies(accountURL))
	d.sessionMu.Lock()
	accountCookieChanged := newAccountCookie != "" && newAccountCookie != d.AccountCookie
	if newAccountCookie != "" {
		d.AccountCookie = newAccountCookie
	}
	d.serviceToken = newSession.serviceToken
	d.userID = newSession.userID
	d.sessionHash = newSession.sessionHash
	d.userHash = newSession.userHash
	d.sessionMu.Unlock()

	if accountCookieChanged && d.ID != 0 {
		op.MustSaveDriverStorage(d)
	}
	log.Infof("[xiaomi_cloud] serviceToken 已自动续签")
	return nil
}

func (s sessionCookies) httpCookies() []*http.Cookie {
	values := map[string]string{
		"serviceToken": s.serviceToken,
		"userId":       s.userID,
		"i.mi.com_slh": s.sessionHash,
		"i.mi.com_ph":  s.userHash,
	}
	cookies := make([]*http.Cookie, 0, len(values))
	for _, name := range []string{"serviceToken", "userId", "i.mi.com_slh", "i.mi.com_ph"} {
		if values[name] != "" {
			cookies = append(cookies, &http.Cookie{Name: name, Value: values[name]})
		}
	}
	return cookies
}

func (s *sessionCookies) apply(cookies []*http.Cookie) bool {
	updated := false
	for _, cookie := range cookies {
		if cookie.Value == "" {
			continue
		}
		switch cookie.Name {
		case "serviceToken":
			updated = updated || s.serviceToken != cookie.Value
			s.serviceToken = cookie.Value
		case "userId":
			updated = updated || s.userID != cookie.Value
			s.userID = cookie.Value
		case "i.mi.com_slh":
			updated = updated || s.sessionHash != cookie.Value
			s.sessionHash = cookie.Value
		case "i.mi.com_ph":
			updated = updated || s.userHash != cookie.Value
			s.userHash = cookie.Value
		}
	}
	return updated
}

func (s sessionCookies) complete() bool {
	return s.serviceToken != "" && s.userID != "" && s.sessionHash != "" && s.userHash != ""
}

func sessionCookiesFromHTTP(cookies []*http.Cookie) sessionCookies {
	var session sessionCookies
	session.apply(cookies)
	return session
}

func parseCookieHeader(cookieString string) []*http.Cookie {
	req := &http.Request{Header: make(http.Header)}
	req.Header.Set("Cookie", cookieString)
	return req.Cookies()
}

func containsCookie(cookies []*http.Cookie, name string) bool {
	for _, cookie := range cookies {
		if cookie.Name == name && cookie.Value != "" {
			return true
		}
	}
	return false
}

func hasCookie(cookieString, name string) bool {
	return containsCookie(parseCookieHeader(cookieString), name)
}

func cookieHeader(cookies []*http.Cookie) string {
	values := make(map[string]string, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name != "" && cookie.Value != "" {
			values[cookie.Name] = cookie.Value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, (&http.Cookie{Name: name, Value: values[name]}).String())
	}
	return strings.Join(pairs, "; ")
}

func isXiaomiAPIURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Hostname(), "i.mi.com")
}

// getFiles 获取指定目录下的文件列表
func (d *XiaomiCloud) getFiles(parentID string) ([]File, error) {
	var allFiles []File
	pageNo := 1
	limit := 100

	for {
		var resp ListResp
		apiURL := fmt.Sprintf("%s%s/folders/children", BaseURL, APIPrefix)

		_, err := d.request(apiURL, http.MethodGet, func(req *resty.Request, _ string) {
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
		_, err := d.request(apiURL, http.MethodGet, func(req *resty.Request, _ string) {
			req.SetQueryParam("taskId", taskID)
		}, &resp)

		if err != nil {
			return err
		}

		if resp.Code != 0 {
			return fmt.Errorf("查询任务失败: %s", resp.Description)
		}

		done, statusErr := evaluateTaskStatus(resp.Data.Status, taskID)
		if statusErr != nil {
			return statusErr
		}
		if done {
			return nil
		}

		// 继续等待
		time.Sleep(interval)
		// 逐渐增加轮询间隔,最大1秒
		if interval < time.Second {
			interval += 100 * time.Millisecond
		}
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
	errMsg := strings.ToLower(err.Error())
	// 网络错误、超时错误等可重试
	return strings.Contains(errMsg, "timeout") ||
		strings.Contains(errMsg, "http错误: 502") ||
		strings.Contains(errMsg, "http错误: 503") ||
		strings.Contains(errMsg, "http错误: 504") ||
		strings.Contains(errMsg, "connection") ||
		strings.Contains(errMsg, "temporary") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "unexpected eof")
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
		return errs.NewErr(errs.EmptyToken, "account_cookie 已失效或需要重新提取")
	}
	if strings.Contains(errMsg, "code=403") || strings.Contains(errMsg, "禁止访问") {
		return fmt.Errorf("访问被拒绝: %w", err)
	}
	if strings.Contains(errMsg, "code=404") || strings.Contains(errMsg, "不存在") {
		return errs.NewErr(errs.ObjectNotFound, "文件或文件夹不存在")
	}
	return err
}
