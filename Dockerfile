FROM golang:1.15-alpine

RUN apk -U --no-cache add build-base git

WORKDIR /go/src/github.com/aeneasr/github-trends

ADD go.mod go.mod
ADD go.sum go.sum

RUN go mod download

ADD . .

RUN CGO_ENABLED=0 go install .

ENTRYPOINT ["github-trends"]
CMD ["serve"]
