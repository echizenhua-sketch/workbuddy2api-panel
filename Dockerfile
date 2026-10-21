# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
# 国内/受限网络下 proxy.golang.org 不可达；GOPROXY 可经 --build-arg 覆盖。
# 仅改变模块获取来源，go.sum 校验强度不变。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
# APK_MIRROR：104 出口到 dl-cdn.alpinelinux.org 不通（IPv4 超时、IPv6 无路由，
# apk 报 "Permission denied"）。默认保持上游 CDN，构建时可 --build-arg 换成
# 内网可达镜像（实测 mirrors.aliyun.com 200/0.6s）。仅换下载源，包校验不变。
ARG APK_MIRROR=dl-cdn.alpinelinux.org
RUN if [ "$APK_MIRROR" != "dl-cdn.alpinelinux.org" ]; then \
      sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories; \
    fi \
 && apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
# /app 必须对运行用户可写：saveConfig 的原子替换会在**同目录**创建
# config.json.tmp（os.WriteFile(path+".tmp") 后 os.Rename）。
# 上游本让 app(10001) 拥有 /app，故本可写；104 部署用 compose 的 user: 覆盖为
# 1011:1011，导致创建 tmp 直接 EACCES，面板「保存配置」自上线起即 400。
#
# 必须是 0777 而非 1777：config.json 是**单文件 bind mount**，rename 覆盖挂载点
# 必失败；上游 saveConfig 对 errno==EBUSY 已有「原地覆写」回退。但实测在 sticky
# 目录下同一 rename 返回的是 EPERM（VFS 先做 sticky 属主校验、再到挂载点判定），
# 上游回退认不出 → 仍报 "operation not permitted"。去掉 sticky 位即恢复 EBUSY，
# 回退得以接手。单用途容器，世界可写风险可控。
RUN chmod 0777 /app
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]