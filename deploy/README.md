# 自建镜像与服务器部署

参考 new-api 的目录与 Actions 部署方式；仅部署 `gpt-load`，不创建数据库或 Redis。部署相关文件集中在 `deploy/`；根目录不再保留上游 `docker-compose.yml`。

## 首次配置

1. 服务器需要 Docker、Docker Compose，以及可执行 Docker 的 SSH 账号。当前工作流与参考项目一致，构建 `linux/amd64` 镜像。
2. 在服务器建立两个独立目录：

   ```sh
   mkdir -p /home/DockerCompose/gpt-load /home/DockerVolumes/gpt-load/data
   ```

   手动将 `deploy/docker-compose.yml` 复制到 `/home/DockerCompose/gpt-load/docker-compose.yml`，将 `deploy/.env.example` 复制为该目录的 `.env`。其中只填 `REGISTRY_REPO`、`VERSION`，每次部署会自动更新。
3. 将 `deploy/app.env.example` 复制到服务器 `/home/DockerVolumes/gpt-load/.env`，设置强随机 `AUTH_KEY`，按需填写其他应用配置。该文件只留在服务器，不放进 GitHub Actions Secrets。保持 `HOST=0.0.0.0`、`PORT=3001`；如修改端口，同步修改服务器 compose 的端口映射。默认 `SERVER_GRACEFUL_SHUTDOWN_TIMEOUT=10`，如修改，同步调整 `stop_grace_period`。
4. `/home/DockerVolumes/gpt-load/data` 挂载到 `/app/data`，保存默认 SQLite 数据和日志。已有部署需先停止旧实例，并将原 `data` 完整迁入此目录，沿用原应用 `.env`（尤其是 `ENCRYPTION_KEY`）。若已有同名容器，切换前移除旧容器，保留数据。不要让两个实例同时操作同一 SQLite 文件。使用外部数据库或 Redis 时，继续在应用 `.env` 填 `DATABASE_DSN` / `REDIS_DSN`，并确保网络可达。
5. 在本地仓库根目录复制 `deploy/.env.secrets.example` 为 `deploy/.env.secrets`，填写镜像仓库、密码式 SSH、`DEPLOY_DIR=/home/DockerCompose/gpt-load` 和 Gmail 通知参数。安装并登录 GitHub CLI 后，在 PowerShell 执行：

   ```powershell
   .\deploy\sync-secrets.ps1
   # 或明确指定你的仓库：
   .\deploy\sync-secrets.ps1 -Repo owner/gpt-load-v1
   ```

   脚本默认读取脚本同目录的 `.env.secrets`，从脚本目录定位 Git 仓库，并写入 `origin` 对应的仓库（不依赖当前工作目录）；可用 `-SecretsFile` 和 `-Repo` 覆盖默认值，运行前核对输出的目标。Gmail 使用应用专用密码。`deploy/.env.secrets` 已被 Git 忽略；不要提交或分享它。`.dockerignore` 保持原样，本地构建镜像前须将真实秘密文件移出构建目录，避免进入构建上下文。

## 发布

将这些配置提交到自己的仓库后，对需要部署的提交创建并推送专用 tag：

```sh
git tag mark-v1.2.3
git push origin mark-v1.2.3
```

- `mark-v1.2.3` → 镜像 `REGISTRY_REPO:v1.2.3`，同时更新该自建仓库的 `latest`
- `mark-v1.2.3-beta.1` → `REGISTRY_REPO:v1.2.3-beta.1`，不更新 `latest`，但仍会部署到同一服务器
- 镜像的前端和 Go 版本均通过 `deploy/Dockerfile` 的 `VERSION` build-arg 注入
- Actions 构建推送后，通过 SSH 在 `DEPLOY_DIR` 更新镜像变量，执行 compose pull/up，等待容器 `/health` 健康检查成功，最后发送 Gmail 结果通知
- 也可在 Actions 手动运行工作流，输入已存在的 `mark-v*` tag
- 普通上游 tag 不触发此部署；三个二进制发布工作流忽略 `mark-v*`

初次手动复制后，工作流不会同步 compose 文件；以后修改部署模板需自行复制到服务器。一次只发布一个 tag，等待部署结束再发下一个。部署失败不会自动回滚；查看 Actions 日志及服务器 `docker compose logs gpt-load`。邮件发送失败不改变部署结果，以 Actions 状态为准。

## 本地开发与镜像构建

在仓库根目录执行 `cp deploy/app.env.example .env`，填写应用配置后使用原有 `make run`；本地运行时 `.env` 与 `Makefile` 仍在根目录。`deploy/.env.example` 只用于服务器 Compose 的镜像选择，不是应用配置。

Docker 构建上下文仍为仓库根目录，命令为 `docker build -f deploy/Dockerfile -t gpt-load:local .`。`.dockerignore` 保持原样，构建前必须将所有真实秘密文件（包括 `deploy/.env.secrets` 和本地 `.env`）移出构建上下文。
