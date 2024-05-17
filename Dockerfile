FROM alpine:3.19 AS builder

ENV GOPROXY=https://goproxy.cn
RUN set -eux && sed -i 's/dl-cdn.alpinelinux.org/mirrors.tuna.tsinghua.edu.cn/g' /etc/apk/repositories
RUN apk add --no-cache iptables
RUN wget -O go.tgz "https://mirrors.ustc.edu.cn/golang/go1.22.1.linux-amd64.tar.gz" \
    && tar -C /usr/local -xzf go.tgz \
    && rm go.tgz
ENV PATH="/usr/local/go/bin:$PATH"

WORKDIR /app

COPY go.mod go.mod
RUN go mod download
COPY . .
RUN go build -o udp2faketcp

FROM alpine:3.19

WORKDIR /app
COPY --from=builder /app/udp2faketcp /app/udp2faketcp
CMD ["/app/udp2faketcp"]