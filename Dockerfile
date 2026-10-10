FROM golang:1.27.1 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 画面の CSS を作る（Tailwind CSS のスタンドアロン CLI と daisyUI。版とチェックサムは scripts/build-css.sh で固定）。
# 取得したファイルはビルドのキャッシュに置いて使い回す。
RUN --mount=type=cache,target=/root/.cache/ossbss-css OSSBSS_CSS_CACHE=/root/.cache/ossbss-css sh scripts/build-css.sh
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /eapaka-ossbss ./cmd/eapaka-ossbss

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /eapaka-ossbss /eapaka-ossbss
ENTRYPOINT ["/eapaka-ossbss"]
CMD ["serve"]
