FROM golang:1.23-alpine AS builder

# ca-certificates: the scratch image below has no root CAs, but the Firestore
# and Firebase Auth clients need them to verify TLS to Google APIs.
RUN apk --no-cache add ca-certificates

WORKDIR /build
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/*.go ./
COPY server/*.html ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /sysrec .

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /sysrec /sysrec

ENV PORT=8080
ENV ARTIFACTS_DIR=/data/artifacts

EXPOSE 8080

ENTRYPOINT ["/sysrec"]
