package xiaomi_cloud

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	// 方式1: 直接粘贴完整Cookie字符串(推荐)
	CookieString string `json:"cookie_string" help:"完整的Cookie字符串(从浏览器开发者工具中复制)"`

	// 方式2: 手动输入各个字段(如果上面的cookie_string为空才使用)
	ServiceToken string `json:"service_token" help:"小米云盘服务令牌"`
	UserID       string `json:"user_id" help:"用户ID"`
	SessionHash  string `json:"session_hash" help:"会话哈希(i.mi.com_slh)"`
	UserHash     string `json:"user_hash" help:"用户哈希(i.mi.com_ph)"`

	// 可选配置
	OrderBy        string `json:"order_by" type:"select" options:"SERVICE_TIME,NAME,SIZE,CREATE_TIME,MODIFY_TIME" default:"SERVICE_TIME" help:"文件排序字段"`
	OrderDirection string `json:"order_direction" type:"select" options:"ASC,DESC" default:"DESC" help:"排序方向"`
	RapidUpload    bool   `json:"rapid_upload" default:"true" help:"启用秒传功能(基于SHA1去重)"`
}

var config = driver.Config{
	Name:        "XiaomiCloud",
	LocalSort:   false,
	OnlyProxy:   false,
	NoCache:     false,
	NoUpload:    false,
	NeedMs:      false,
	DefaultRoot: "0",
	CheckStatus: false,
	Alert: `info|小米云盘存储驱动

配置方式（二选一）：

方式1（推荐）- 直接粘贴Cookie：
1. 登录 https://i.mi.com/drive/h5
2. 打开浏览器开发者工具(F12)
3. 切换到 Network/网络 标签页
4. 刷新页面，点击任意请求
5. 找到请求头(Request Headers)中的 Cookie 字段
6. 复制完整的Cookie值，粘贴到 cookie_string 字段即可

方式2（手动提取）：
如果方式1不可用，可以手动从Cookie中提取以下4个字段：
- service_token: serviceToken 的值
- user_id: userId 的值
- session_hash: i.mi.com_slh 的值
- user_hash: i.mi.com_ph 的值

特性：
- ✅ 支持秒传功能(相同SHA1的文件无需重复上传)
- ✅ 支持大文件分块上传(4MB块)
- ✅ 支持CDN加速下载
- ✅ 支持完整的文件管理操作`,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &XiaomiCloud{}
	})
}
