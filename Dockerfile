# paper-rag-backend（Go + Gin）
# 多阶段：
#   阶段1 构建 Go 二进制（golang:1.26）
#   阶段2 运行时（python:3.12-slim：Go 服务 + PyMuPDF 公式截图渲染）
FROM golang:1.26 AS builder
WORKDIR /app
# 国内 Go 依赖代理（proxy.golang.org 在国内被墙/超时）
ENV GOPROXY=https://goproxy.cn,direct
# go.mod 里 replace 了本地 fork（third_party/pdf），go mod download 前必须 COPY
COPY go.mod go.sum ./
COPY third_party/ ./third_party/
RUN go mod download
COPY . .
# 注意：gojieba 通过 cgo 封装 C++ 的 cppjieba，必须启用 CGO（不能 CGO_ENABLED=0）
RUN go build -o /server ./cmd/server

# 运行时：用 python slim（自带 python3，供 render 调 PyMuPDF 渲染公式截图）
FROM python:3.12-slim
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources 2>/dev/null || \
    sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl libmagic1 libstdc++6 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir -i https://mirrors.aliyun.com/pypi/simple/ pymupdf

WORKDIR /app
COPY --from=builder /server ./server
# gojieba 运行时通过 runtime.Caller 定位编译路径的 deps/cppjieba/dict 加载词典，
# 必须把词典复制到与编译期一致的绝对路径（/go/pkg/mod/...）
COPY --from=builder /go/pkg/mod/github.com/yanyiwu/gojieba@v1.4.7/deps/cppjieba/dict \
    /go/pkg/mod/github.com/yanyiwu/gojieba@v1.4.7/deps/cppjieba/dict
# 配置文件：clone 后手动从 example 复制（含 ${ENV} 占位符，运行时由 env 注入展开）
COPY config/config.example.yaml ./config/config.yaml
COPY db/init.sql ./db/init.sql

EXPOSE 8080
CMD ["./server", "-config", "config/config.yaml"]
