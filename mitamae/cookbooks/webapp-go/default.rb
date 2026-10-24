# Go 版 webapp を /home/isucon/webapp-go/ に配置してビルドし、Rust 版の代わりに :80 で起動する。
#
# Go 版は SQL (schema.sql / seed.sql) と SPA を Rust 版のディレクトリから読むので、
# webapp cookbook の後に適用し、/home/isucon/webapp はそのまま残す。

remote_directory '/home/isucon/webapp-go' do
  source 'files/home/isucon/webapp-go'
  owner 'isucon'
  group 'isucon'
end

execute 'build webapp-go' do
  command 'sudo -u isucon -H bash -lc "cd /home/isucon/webapp-go && go build -o webapp-go ."'
end

# Rust 版と同じく :80 を非 root で bind するため CAP_NET_BIND_SERVICE を渡す。
# PATH の /usr/bin は /api/initialize が呼ぶ mysql CLI のため。
file '/etc/systemd/system/nrb2026-webapp-go.service' do
  owner 'root'
  group 'root'
  mode '0644'
  content <<~UNIT
    [Unit]
    Description=nrb2026 webapp (Go, listens on :80)
    After=network-online.target mysql.service
    Wants=network-online.target

    [Service]
    Type=simple
    User=isucon
    Group=isucon
    WorkingDirectory=/home/isucon/webapp-go
    Environment=PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
    Environment=DATABASE_URL=mysql://isucon:isucon@127.0.0.1:3306/nrb2026
    Environment=PORT=80
    Environment=STATIC_DIR=/home/isucon/webapp/public
    Environment=SQL_DIR=/home/isucon/webapp/sql
    AmbientCapabilities=CAP_NET_BIND_SERVICE
    CapabilityBoundingSet=CAP_NET_BIND_SERVICE
    NoNewPrivileges=true
    ExecStart=/home/isucon/webapp-go/webapp-go
    Restart=on-failure
    RestartSec=2

    [Install]
    WantedBy=multi-user.target
  UNIT
end

execute 'systemctl daemon-reload'

# :80 を取り合わないよう、Go 版を起動する前に Rust 版を止める。
service 'nrb2026-webapp' do
  action [:stop, :disable]
end

service 'nrb2026-webapp-go' do
  action [:enable, :start]
end
