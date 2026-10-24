# nrb2026 default role
#
# 適用順序:
#   common      → apt 更新、sysctl、SSH 設定
#   users       → isucon ユーザー作成
#   mysql       → MySQL 8.0 install + nrb2026 db + isucon user
#   rustup      → rustup バイナリと toolchain 投入
#   webapp      → webapp ソース配置 + 事前 cargo build + systemd unit (cargo run debug, :80)
#   go          → Go toolchain を /usr/local/go に投入
#   webapp-go   → Go 版 webapp 配置 + go build + systemd unit (:80、Rust 版の unit は disable)
#   bench       → bench バイナリ + bench.sh 配置
#
# nginx / tls cookbook は nrb2026 では使わない (= webapp が axum で 80 番直配信、TLS なし)。
# 「アプリで static file を配るのは遅い」ことに参加者が気づき、nginx 等を導入する流れ自体が
# 改善経路の 1 本になっている。詳細: docs/authoring/platform.md 付録 A。
#
# 残り placeholder cookbook (docker/envoy/nftables/ipaddr) は順次有効化する。
ENV['DEBIAN_FRONTEND'] = 'noninteractive'

include_recipe '../cookbooks/common/default.rb'
include_recipe '../cookbooks/users/isucon.rb'
include_recipe '../cookbooks/mysql/default.rb'
include_recipe '../cookbooks/rustup/default.rb'
include_recipe '../cookbooks/webapp/default.rb'
include_recipe '../cookbooks/go/default.rb'
include_recipe '../cookbooks/webapp-go/default.rb'
include_recipe '../cookbooks/bench/default.rb'
