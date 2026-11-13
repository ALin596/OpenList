package xiaomi_cloud

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	PassToken      string `json:"pass_token" required:"true" help:"填写 passToken 的值；服务端轮换后会自动更新并保存"`
	UserID         string `json:"user_id" required:"true" help:"填写与 passToken 对应的 userId，无需填写 Cookie 名称或 deviceId"`
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
	NoLinkURL:         true,
	Alert: `info|小米云盘存储驱动

配置方式：
1. 打开浏览器开发者工具的 Network/网络 标签页并保留日志
2. 登录 https://i.mi.com/drive/h5
3. 在 account.xiaomi.com/pass/serviceLoginAuth2 登录成功的响应 Cookie 中获取 passToken 和 userId
4. 将这两个 Cookie 的值分别填入 Pass token 和 User id 输入框，无需填写 Cookie 名称或 deviceId
5. 服务端轮换 passToken 后会自动保存，重新打开配置页面可查看最新值
6. 优先复用内存中的 serviceToken；首次启动或收到 HTTP 401 时，使用账号凭据获取新的云盘会话

特性：
- 自动获取并续签小米云盘 serviceToken
- 支持基于SHA1的秒传
- 支持大文件4MB分块并发上传
- 支持CDN加速下载
- 支持通过POST下载大文件
- 支持完整的文件管理操作`,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &XiaomiCloud{}
	})
}
