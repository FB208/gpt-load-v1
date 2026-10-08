# 自建镜像与服务器部署

参考 new-api 的目录与 Actions 部署方式；仅部署 `gpt-load`，不创建数据库或 Redis。部署相关文件集中在 `deploy/`；根目录不再保留上游 `docker-compose.yml`。

支持部署到多台服务器：每台服务器对应一个同名的 GitHub Environment，SSH 连接信息存在各自环境的 Secrets 里；镜像仓库和邮件通知是仓库级 Secrets，所有服务器共用。各服务器独立运行，互不共享数据。

## 首次配置

第 1～4 步在每台服务器上各做一遍。

1. 服务器需要 Docker、Docker Compose，以及可执行 Docker 的 SSH 账号。当前工作流与参考项目一致，构建 `linux/amd64` 镜像。
2. 在服务器建立两个独立目录：

   ```sh
   mkdir -p /home/DockerCompose/gpt-load /home/DockerVolumes/gpt-load/data
   ```

   手动将 `deploy/docker-compose.yml` 复制到 `/home/DockerCompose/gpt-load/docker-compose.yml`，将 `deploy/.env.example` 复制为该目录的 `.env`。其中只填 `REGISTRY_REPO`、`VERSION`，每次部署会自动更新。
3. 将 `deploy/app.env.example` 复制到服务器 `/home/DockerVolumes/gpt-load/.env`，设置强随机 `AUTH_KEY`，按需填写其他应用配置。该文件只留在服务器，不放进 GitHub Actions Secrets。保持 `HOST=0.0.0.0`、`PORT=3001`；如修改端口，同步修改服务器 compose 的端口映射。默认 `SERVER_GRACEFUL_SHUTDOWN_TIMEOUT=10`，如修改，同步调整 `stop_grace_period`。
4. `/home/DockerVolumes/gpt-load/data` 挂载到 `/app/data`，保存默认 SQLite 数据和日志。已有部署需先停止旧实例，并将原 `data` 完整迁入此目录，沿用原应用 `.env`（尤其是 `ENCRYPTION_KEY`）。若已有同名容器，切换前移除旧容器，保留数据。不要让两个实例同时操作同一 SQLite 文件。使用外部数据库或 Redis 时，继续在应用 `.env` 填 `DATABASE_DSN` / `REDIS_DSN`，并确保网络可达。各服务器的数据库和 Redis 也必须彼此独立，保持 `IS_SLAVE=false`：本工作流不处理多台共享数据所需的主从集群和发布顺序。
5. 在本地仓库根目录复制 `deploy/.env.secrets.example` 为 `deploy/.env.secrets` 并填写：节外是共用的镜像仓库和 Gmail 通知参数；每台服务器写一个 `[env:名字]` 节，填该机的 `SSH_HOST`、`SSH_USER`、`SSH_PASSWORD`、`DEPLOY_DIR`（`SSH_PORT` 可省略，默认 22）。这几个服务器键写在节外会直接报错。安装并登录 GitHub CLI 后，在 PowerShell 执行：

   ```powershell
   .\deploy\sync-secrets.ps1
   # 或明确指定你的仓库：
   .\deploy\sync-secrets.ps1 -Repo owner/gpt-load-v1
   ```

   脚本默认读取脚本同目录的 `.env.secrets`，从脚本目录定位 Git 仓库，并写入 `origin` 对应的仓库（不依赖当前工作目录）；可用 `-SecretsFile` 和 `-Repo` 覆盖默认值，运行前核对输出的目标。脚本先完整校验文件，再依次：
   - 写入仓库级 Secrets；
   - 为每个节创建同名 Environment（已存在则复用），新建的环境只允许 `mark-v*` tag 和默认分支部署，已有环境的保护规则不覆盖，未限制时给出警告；
   - 写入各环境的 Secrets；
   - 按节的顺序把环境名写入仓库变量 `DEPLOY_TARGETS`（如 `["prod-1","prod-2"]`），它决定部署哪些服务器、按什么顺序；
   - 若仓库级还残留 `SSH_*` / `DEPLOY_DIR`，打印删除命令。环境漏配某个 secret 时，GitHub 会静默改用仓库级同名值，可能把部署发到错误的服务器，所以确认环境部署正常后要删掉它们。

   仓库公开，环境名和 `DEPLOY_TARGETS` 所有人可见，用 `prod-1` 这类中性名字，不要用 IP 或域名命名。Gmail 使用应用专用密码。`deploy/.env.secrets` 已被 Git 忽略；不要提交或分享它。`.dockerignore` 保持原样，本地构建镜像前须将真实秘密文件移出构建目录，避免进入构建上下文。

### 从单服务器配置迁移

旧版把 `SSH_*`、`DEPLOY_DIR` 写在仓库级 Secrets。迁移步骤：

1. 在 `deploy/.env.secrets` 的 `SSH_HOST` 等五行之前加一行 `[env:prod-1]`（名字自定）。节头之后直到下一个节头的所有键都写入该环境，所以共用的镜像仓库和 Gmail 配置必须留在第一个节头之前。
2. 执行 `.\deploy\sync-secrets.ps1`。
3. 发一次 tag（或手动运行工作流），确认部署到 `prod-1` 成功。
4. 按脚本提示执行 `gh secret delete SSH_HOST --repo owner/gpt-load-v1` 等命令，删除仓库级 `SSH_HOST`、`SSH_PORT`、`SSH_USER`、`SSH_PASSWORD`、`DEPLOY_DIR`。

### 增减服务器

- 增加：新服务器按第 1～4 步准备好，在 `deploy/.env.secrets` 追加一个 `[env:名字]` 节，重新执行同步脚本。
- 减少：从文件删掉对应的节，重新执行同步脚本，`DEPLOY_TARGETS` 会随之更新；GitHub 上的 Environment 不会自动删除，不需要时在 Settings → Environments 手动删除。

## 发布

将这些配置提交到自己的仓库后，对需要部署的提交创建并推送专用 tag：

```sh
git tag mark-v1.2.3
git push origin mark-v1.2.3
```

- `mark-v1.2.3` → 镜像 `REGISTRY_REPO:v1.2.3`，同时更新该自建仓库的 `latest`
- `mark-v1.2.3-beta.1` → `REGISTRY_REPO:v1.2.3-beta.1`，不更新 `latest`，但仍会部署到 `DEPLOY_TARGETS` 中的所有服务器
- 镜像的前端和 Go 版本均通过 `deploy/Dockerfile` 的 `VERSION` build-arg 注入
- Actions 构建推送后，按 `DEPLOY_TARGETS` 的顺序逐台部署：通过 SSH 在该机的 `DEPLOY_DIR` 更新镜像变量，执行 compose pull/up，等待容器 `/health` 健康检查成功后再部署下一台；全部结束后发送 Gmail 结果通知
- 也可在 Actions 手动运行工作流，输入已存在的 `mark-v*` tag；`targets` 填逗号分隔的环境名（如 `prod-2`）只部署这几台，留空部署全部。环境名必须在 `DEPLOY_TARGETS` 中，否则构建前就会报错
- 普通上游 tag 不触发此部署；三个二进制发布工作流忽略 `mark-v*`

初次手动复制后，工作流不会同步 compose 文件；以后修改部署模板需自行复制到每台服务器。一次只发布一个 tag，等待部署结束再发下一个。某台部署失败时，后面的服务器不再部署，已部署成功的也不会回滚，此时各服务器版本不一致：查看 Actions 日志和该服务器的 `docker compose logs gpt-load`，修复后重新发布，或手动运行工作流并用 `targets` 只补部署剩下的服务器。邮件发送失败不改变部署结果，以 Actions 状态为准；邮件里的部署结果是所有服务器的汇总，每台的情况看运行链接。

## 本地开发与镜像构建

在仓库根目录执行 `cp deploy/app.env.example .env`，填写应用配置后使用原有 `make run`；本地运行时 `.env` 与 `Makefile` 仍在根目录。`deploy/.env.example` 只用于服务器 Compose 的镜像选择，不是应用配置。

Docker 构建上下文仍为仓库根目录，命令为 `docker build -f deploy/Dockerfile -t gpt-load:local .`。`.dockerignore` 保持原样，构建前必须将所有真实秘密文件（包括 `deploy/.env.secrets` 和本地 `.env`）移出构建上下文。
