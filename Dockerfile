# paper-rag-backend（Go + Gin）
# 多阶段：
#   阶段1 构建 Go 二进制（golang:1.26）
#   阶段2 运行时（python:3.12-slim：Go 服务 + PyMuPDF 公式截图渲染）
FROM golang:1.26 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

# 运行时：用 python slim（自带 python3，供 render 调 PyMuPDF 渲染公式截图）
FROM python:3.12-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl libmagic1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir pymupdf

WORKDIR /app
COPY --from=builder /server ./server
# 配置文件：clone 后手动从 example 复制（含 ${ENV} 占位符，运行时由 env 注入展开）
COPY config/config.example.yaml ./config/config.yaml
COPY db/init.sql ./db/init.sql

EXPOSE 8080
CMD ["./server", "-config", "config/config.yaml"]
