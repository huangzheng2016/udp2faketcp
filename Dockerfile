FROM alpine:3.19 AS builder

ENV GOPROXY=https://goproxy.cn
RUN set -eux && sed -i 's/dl-cdn.alpinelinux.org/mirrors.tuna.tsinghua.edu.cn/g' /etc/apk/repositories
RUN apk add --no-cache iptables
RUN wget -O go.tgz "https://mirrors.ustc.edu.cn/golang/go1.26.8.linux-amd64.tar.gz" \
    && tar -C /usr/local -xzf go.tgz \
    && rm go.tgz
ENV PATH="/usr/local/go/bin:$PATH"

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o udp2faketcp ./cmd/udp2faketcp

FROM alpine:3.19

RUN set -eux && sed -i 's/dl-cdn.alpinelinux.org/mirrors.tuna.tsinghua.edu.cn/g' /etc/apk/repositories \
    && apk add --no-cache iptables

WORKDIR /app
COPY --from=builder /app/udp2faketcp /app/udp2faketcp
CMD ["/app/udp2faketcp"]
