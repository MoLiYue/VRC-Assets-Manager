# MioVRCA new

Windows 上的 VRChat 改模素材管理工具。把本地文件夹、Booth / Gumroad 已购和网盘分享里的素材整理成一个素材库，一键导入 Unity 工程，再用自动化流水线装配到模型上。

**这是修改版**：基于 [MioVRCA](https://github.com/CokoIya/MioVRC_AssetManager)（作者：天川澪），不是原作者发布的版本，使用中的问题请不要反馈给原作者。原项目只公开了部分源码，本项目补齐了后端，并按自己的版本号（从 0.0.1 开始）发布。

## 功能

- **素材库**：扫描素材文件夹、unitypackage 和压缩包，自动分类、识别适配素体、关联 Booth 商品；同款合并、风格标签、搜索筛选；标出每个素材用在哪些 Unity 工程里。
- **购买和下载**：在软件里逛 Booth，同步并下载 Booth、Gumroad 已购（点「同步」时只读取新增的已购）；读取并下载百度网盘、Google Drive、Dropbox 分享；闲鱼在独立窗口中打开。
- **工程**：一键导入素材（自动解压，按素体选包，跳过重复）；一键新建 VRChat 基础工程；上传前体检和一键处理。
- **自动化流水线**：把衣服、头发、配饰、道具装配到模型上，按层级生成菜单和图标；可以接 DeepSeek、通义千问、Claude 等 AI 整理菜单，或在对话中让 AI 继续改模；装配结果可以保存为方案，在其他工程上一键应用。
- 界面支持中文、English、日本語。

## 和 MioVRCA 的关系

- 名称、数据文件夹（`%LOCALAPPDATA%\MioVRC_AssetManager_new`）、安装位置、注册表项和本地端口都与 MioVRCA 分开，两个软件可以同时安装、同时运行，互不影响。
- 安装版第一次启动时，如果本机有 MioVRCA 的数据，会复制一份过来（素材库、设置、封面、登录信息等），MioVRCA 的数据保持不变。
- Unity 插件（`com.miovrc.pipeline`、`com.miovrc.projectcard`）沿用原项目的包名，已装过插件的工程可以直接使用。
- 目前还没有更新渠道，软件不会自动检查更新。

## 从源码构建

需要 Go 1.24 或更新版本，第三方库已经放在 `third_party/` 里：

```bat
go test ./...
go build -trimpath -ldflags "-H windowsgui -s -w" -o dist\MioVRC_AssetManager_new.exe ./cmd/miovrca
```

在 Linux / WSL 上用 `sh packaging/build.sh` 生成安装版和便携版（需要 NSIS 3）。推送到 GitHub 后，`.github/workflows/build.yml` 会自动测试并打包；推送 `v` 开头的标签时创建草稿 Release。

## 说明

- 本工具与 VRChat、BOOTH、pixiv、闲鱼、百度网盘均无关联，读取的是这些网站的网页和公开接口，网站改版后部分功能可能失效。
- 素材库数据只保存在本机，登录信息加密保存。
- 网盘下载只使用你自己账号的正常权限，不绕过限速，验证码由你自己输入。

## 许可证

**GPL-3.0**，并附带原项目的附加条款：界面中保留「基于 MioVRCA（作者：天川澪）」和原项目地址，标明是修改版，使用不同的名称和图标。详见 [LICENSE.md](LICENSE.md)。

## 第三方代码

| 项目 | 许可证 |
| --- | --- |
| [MioVRCA](https://github.com/CokoIya/MioVRC_AssetManager)（本项目的原项目） | GPL-3.0 或 PolyForm Noncommercial 1.0.0，本项目按 GPL-3.0 |
| [jchv/go-webview2](https://github.com/jchv/go-webview2) | MIT（有少量修改，见 `third_party/go-webview2/VRCLIB_PATCHES.txt`） |
| [jchv/go-winloader](https://github.com/jchv/go-winloader) | ISC |
| [golang.org/x/sys](https://go.googlesource.com/sys) | BSD-3-Clause |
| WebView2Loader.dll（Microsoft WebView2 SDK） | 见 `third_party/go-webview2/webviewloader/sdk/LICENSE.txt` |
| [UnitySkills](https://github.com/Besty0728/Unity-Skills) 2.8.4（`unitykit/`，原样打包，去掉了 Tests） | MIT，见 `unitykit/UnitySkills-LICENSE.txt` |

Booth 已购页面的读取方式参考了 [BoothDownloader](https://github.com/Myrkie/BoothDownloader) 和 [booth-library-manager](https://github.com/yoshiki-0428/booth-library-manager)，百度网盘分享的读取方式参考了 [AList 的百度分享驱动](https://alistgo.com/guide/drivers/baidu.share.html)，转存和下载参考了 [BaiduPCS-Go](https://github.com/qjfoidnh/BaiduPCS-Go) 和 [BaiduPCS-Py](https://github.com/PeterDing/BaiduPCS-Py)，穿戴和菜单靠玩家工程里的 [Modular Avatar](https://modular-avatar.nadena.dev/)（不随软件分发）。
