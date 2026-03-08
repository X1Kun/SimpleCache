# 1. 编译代码
FROM golang:1.24-alpine AS builder
WORKDIR /app
# # 配置国内环境
ENV GOPROXY=https://goproxy.cn,direct
# 拷贝当前目录下所有代码
COPY . . 
# 编译成名为 geecache-server 的二进制文件
RUN go build -o geecache-server main.go

# 2. 运行环境
FROM alpine:latest
WORKDIR /app
# 把阶段 1 编译好的二进制文件拿过来
COPY --from=builder /app/geecache-server .
# 默认启动命令
CMD ["./geecache-server"]