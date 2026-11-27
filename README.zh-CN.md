# openlinker-registry

独立的 Agent Skill 与 MCP 服务仓库。一个二进制、一个 PostgreSQL 数据库、一个存放内容的目录。
完整的配置表、接口表和规则见 [README.md](README.md)（英文），本页是中文概览。

> 状态：本仓库自己维护版本号，见[发布](#发布)和 [CHANGELOG.md](CHANGELOG.md)。未包含的功能见文末。

## 它做什么

**自己存储**

| 类型 | 方式 | 含义 |
|---|---|---|
| Skill | 托管 | 上传，或从 GitHub 固定提交导入；版本不可变，按 sha256 寻址 |
| Skill | 收录 | 只记录来源地址、文件清单和摘要，内容留在源头（GitHub 目录，或其他站点发现索引里的条目） |
| MCP 服务 | 注册 / 收录 | 保存 `server.json`，带版本和状态；收录表示条目不是服务作者本人发布的 |

**转发到其他仓库并镜像（上游）**

| 上游类型 | 对象 | 镜像内容 |
|---|---|---|
| `openlinker-registry` | 另一个 openlinker-registry | Skill（`/v1`）和 MCP 服务（`/v0.1`） |
| `mcp-registry` | 任何实现官方 MCP Registry 只读接口的仓库，包括 `registry.modelcontextprotocol.io` | MCP 服务 |
| `agent-skills` | 发布 `/.well-known/agent-skills/index.json` 的站点 | 其中的 Skill，放进本地指定的命名空间；索引当前给出的摘要对应的版本就是最新版本（站点改回早先内容时也一样）；文件只换地址、内容不变时，新地址校验通过后改从新地址读取 |

- **未命中时转发**（`proxy_on_miss`）：按名称读取一个本地没有的条目（或镜像 Skill 的新版本）时，按优先级向上游查询，并把结果镜像下来。只有本来就能看到该上游镜像内容的调用者才会触发转发，匿名调用者不会让仓库去访问镜像为 `internal` 的上游。
- **定时同步**（`sync_interval_seconds`）：把上游目录镜像到本地，列表和搜索因此完整。MCP 用 `updated_since` 增量同步；状态变更、任意版本的撤回和删除都会传播。已记录的 MCP 版本内容不变；上游对应文档损坏或内容不同，它的状态和服务器可见性仍然跟随上游。上游列表读不完整时，不会据此判定任何条目已被移除。
- **镜像不会比上游更公开**：上游是 openlinker-registry 时，`internal` 条目保持 `internal`，未列出的保持未列出，`private` 条目不镜像。收紧总是先于新内容生效，转发读取时和同步时都一样。允许/拒绝名单不再选中的命名空间，其已镜像条目在下次同步时撤回，重新选中后恢复。上游不再共享的条目会被**撤回**：在本地变为仅管理员可见（包括已缓存的字节），上游重新共享后自动恢复。
- **镜像条目在本地只读**（管理员也不能改），一切跟随上游。另一台 openlinker-registry 必须按 `openlinker-registry` 类型配置；按 `mcp-registry` 配置时同步会被拒绝，因为官方变更流不携带可见性。
- **内容只进不出**：本地存储的内容永远不会推送给上游。
- **字节按需拉取并校验**：镜像 Skill 时只记元数据，第一次读取时才从上游取字节，核对 sha256 摘要和文件清单后缓存；不一致就记为 `source_changed`，不返回任何内容。
- **归属**：镜像条目保留上游的命名空间，该命名空间在本地只读，且只属于这一个上游。本地已有的命名空间不会被转发或覆盖。

**服务标准客户端**

- `npx skills add <地址>` 通过发现索引安装 Skill。
- `/v0.1`（以及 `/v0`）实现官方 MCP Registry 的只读接口，官方生态的客户端和下游仓库可以直接使用。

**不做的事**：不运行 MCP 服务，不执行 Skill 内容，不保存认证头等机密值（带凭据的 `server.json` 会被拒绝，镜像来的会先去掉凭据），不为内容的安全性背书。

## 快速开始（私有实例）

```sh
cd deploy
cp env.example .env        # 填写 REGISTRY_POSTGRES_PASSWORD 和 REGISTRY_ADMIN_TOKEN
docker compose up -d --build
```

打开 `http://localhost:8080/ui/`，用管理员令牌登录。下面 `ADMIN` 是管理员令牌，`R=http://localhost:8080`：

```sh
# 建命名空间，签发发布令牌（令牌只显示一次）
curl -sS -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"internal.team","kind":"internal"}' $R/v1/admin/namespaces
curl -sS -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"ci","scopes":["skills:read","skills:write"],"roles":[{"namespace":"internal.team","role":"publisher"}]}' \
  $R/v1/admin/tokens

# 存一个 Skill 和一个 MCP 服务
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/markdown' \
  --data-binary @SKILL.md "$R/v1/skills/internal.team/my-skill/versions?version=1.0.0"
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data-binary @server.json $R/v1/mcp/servers/internal.team

# 转发并镜像官方 MCP Registry
curl -sS -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"official","kind":"mcp-registry","base_url":"https://registry.modelcontextprotocol.io",
       "proxy_on_miss":true,"latest_only":true,"sync_interval_seconds":3600}' $R/v1/admin/upstreams
```

上游需要令牌时，用环境变量 `REGISTRY_UPSTREAM_TOKEN_<上游名>` 提供（名称大写，`-` 换成 `_`）。令牌不入库，只发给该上游自己的源。
上游在内网时，用 `REGISTRY_UPSTREAM_ALLOW_CIDRS` 放行对应网段；默认只允许 https 和公网地址。

## 公开实例与私有实例

| 项目 | 公开（`REGISTRY_MODE=public`） | 私有（默认） |
|---|---|---|
| 匿名读取 | 开启 | 关闭，需要令牌 |
| 默认可见性 | `public` | `internal` |
| 自带网页 `/ui/` | 默认关闭（由前面的站点提供界面） | 默认开启，中英文，无脚本 |

可见性三档：`public` 任何人可读，`internal` 本实例的登录用户可读，`private` 仅命名空间成员可读。
不可见的条目在所有读取出口都返回 404。镜像条目只能是 `public` 或 `internal`。

## 发布

本仓库自己维护版本号，和 OpenLinker 的其他组件仓库一样。向本仓库推送一个 `v*` tag 就是一次发布，
它触发 `.github/workflows/release.yml`：

1. 先在该提交上跑 gofmt、`go vet` 和测试（含 PostgreSQL），不通过就什么都不发布。
2. 推送 `linux/amd64` 与 `linux/arm64` 镜像到 `ghcr.io/openlinker-ai/openlinker-registry`，标签是
   `sha-<提交前 12 位>` 和版本 tag；只有不带预发布后缀的 tag 才会移动 `latest`。
3. 把 Linux、macOS、Windows（amd64 与 arm64）的归档和各自的 `.sha256` 文件挂到同名的 GitHub release；
   带后缀的 tag（如 `v0.1.0-rc.1`）发布为预发布版。

`openlinker-registry version` 打印版本和提交。普通的本地构建（`make build`、`docker build`）显示 `dev`。
只有 tag 推送才算发布：手动运行工作流（无论在分支还是 tag 上）只推送 `sha-` 镜像，归档留作工作流产物，
两者的版本都是 `sha-<提交前 12 位>`；CI 冒烟打的归档版本是 `ci`。

发布前的检查清单，以及固定本仓库提交的 OpenLinker 主仓库与这些 tag 的关系，见 [RELEASE.md](RELEASE.md)
（英文）。实际发布过哪些版本，以 [CHANGELOG.md](CHANGELOG.md)、本仓库的 Releases 页面和镜像列表为准。

## 审核、举报与认领

公开模式默认开启审核（`REGISTRY_REVIEW`）：未受信任命名空间的新条目、所有收录条目及其每个新版本都先进入审核，
审核中的条目只有命名空间成员（不含网关被共享的角色）和管理员能读，复制元数据里显示为 `private`，下游镜像不会复制。
登录用户可以举报，不同举报者达到 `REGISTRY_REPORT_THRESHOLD`（默认 3）时条目回到审核。
网关用户可以领取 `ai.openlinker.<handle>` 或 `io.github.<login>`，并认领从自己个人 GitHub 仓库收录的条目
（按收录时记录的仓库数字 ID 校验）；旧地址 308 跳转并永久保留。接口细节见英文 README 的
“Review, reports and claims”。

## 未包含

Registry 自身的 GitHub 登录、经网关的组织命名空间、GitHub Actions OIDC、
企业 OIDC 单点登录、DNS/HTTP 命名空间验证（以及按域名认领 MCP 远程地址）、认领时合并到已有条目、条目级共享、离线包、发布者签名、
S3 存储（接口已有，内容在本地磁盘，一个存储目录只能由一个进程使用）、多副本共享的限流、
手工收录单个任意 URL（发现索引以上游方式同步）、网页上的写操作。

## 测试

`go test ./...` 运行单元测试和端到端测试（内存存储，假 GitHub 与假上游，不访问网络）。
设置 `REGISTRY_TEST_DATABASE_URL` 后，同一套测试也会在 PostgreSQL 上运行。

## 许可证

Apache-2.0
