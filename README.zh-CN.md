# openlinker-registry

独立的 Agent Skill 与 MCP 服务仓库。一个二进制、一个 PostgreSQL 数据库、一个存放内容的目录。
完整的配置表、接口表和规则见 [README.md](README.md)（英文），本页是中文概览。

> 状态：仅源码，尚未发布。未包含的功能见文末。

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
| `agent-skills` | 发布 `/.well-known/agent-skills/index.json` 的站点 | 其中的 Skill，放进本地指定的命名空间 |

- **未命中时转发**（`proxy_on_miss`）：按名称读取一个本地没有的条目（或镜像 Skill 的新版本）时，按优先级向上游查询，并把结果镜像下来。只有本来就能看到该上游镜像内容的调用者才会触发转发，匿名调用者不会让仓库去访问镜像为 `internal` 的上游。
- **定时同步**（`sync_interval_seconds`）：把上游目录镜像到本地，列表和搜索因此完整。MCP 用 `updated_since` 增量同步；状态变更、任意版本的撤回和删除都会传播。
- **镜像不会比上游更公开**：上游是 openlinker-registry 时，`internal` 条目保持 `internal`，未列出的保持未列出，`private` 条目不镜像。收紧总是先于新内容生效，转发读取时和同步时都一样。上游不再共享的条目会被**撤回**：在本地变为仅管理员可见（包括已缓存的字节），上游重新共享后自动恢复。
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

## 未包含

OpenLinker Core 接入（目录代理、网关断言、导入 Core）、GitHub 登录与命名空间自助、GitHub Actions OIDC、
企业 OIDC 单点登录、DNS/HTTP 命名空间验证、认领与共享、审核队列与举报、离线包、发布者签名、
S3 存储（接口已有，内容在本地磁盘，一个存储目录只能由一个进程使用）、多副本共享的限流、
手工收录单个任意 URL（发现索引以上游方式同步）、网页上的写操作。

## 测试

`go test ./...` 运行单元测试和端到端测试（内存存储，假 GitHub 与假上游，不访问网络）。
设置 `REGISTRY_TEST_DATABASE_URL` 后，同一套测试也会在 PostgreSQL 上运行。

## 许可证

Apache-2.0
