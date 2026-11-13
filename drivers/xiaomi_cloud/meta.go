package xiaomi_cloud

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	AccountCookie  string `json:"account_cookie" required:"true" type:"text" help:"填写 passToken=令牌; userId=账号ID；无需 deviceId，自动续签不会改写此配置"`
	OrderBy        string `json:"order_by" type:"select" options:"SERVICE_TIME,NAME,SIZE,CREATE_TIME,MODIFY_TIME" default:"SERVICE_TIME" help:"文件排序字段"`
	OrderDirection string `json:"order_direction" type:"select" options:"ASC,DESC" default:"DESC" help:"排序方向"`
	UploadThread   int    `json:"upload_thread" type:"number" default:"4" help:"分块上传并发数，范围 1-32"`
}

var config = driver.Config{
	Name:              "XiaomiCloud",
	LocalSort:         false,
	OnlyProxy:         false,
	NoCache:           false,
	NoUpload:          false,
	NeedMs:            false,
	DefaultRoot:       "0",
	CheckStatus:       false,
	NoOverwriteUpload: true,
	Alert: `info|小米云盘存储驱动

配置方式：
1. 打开浏览器开发者工具的 Network/网络 标签页并保留日志
2. 登录 https://i.mi.com/drive/h5
3. 在 account.xiaomi.com/pass/serviceLoginAuth2 登录成功的响应 Cookie 中获取 passToken 和 userId
4. 按 passToken=令牌; userId=账号ID 的格式填写 account_cookie，无需 deviceId
5. serviceToken 由驱动自动获取和续签；账号 Cookie 配置保持原样，服务端补发的 Cookie 仅保存在运行时内存中

特性：
- 自动获取并续签小米云盘 serviceToken
- 支持基于SHA1的秒传
- 支持大文件4MB分块并发上传
- 支持CDN加速下载
- 支持完整的文件管理操作`,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &XiaomiCloud{}
	})
}
