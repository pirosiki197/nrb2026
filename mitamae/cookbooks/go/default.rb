# Go toolchain を go.dev の tarball から /usr/local/go に入れる。
# Ubuntu 24.04 の golang-go (1.22) は webapp-go の go.mod (go 1.23) を満たさない。

GO_VERSION = '1.27.1'
GO_SHA256 = '63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445'

execute "install go #{GO_VERSION}" do
  command <<~SH
    set -e
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    curl -fsSL -o "$tmp/go.tar.gz" "https://go.dev/dl/go#{GO_VERSION}.linux-amd64.tar.gz"
    echo "#{GO_SHA256}  $tmp/go.tar.gz" | sha256sum -c -
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tmp/go.tar.gz"
  SH
  not_if "/usr/local/go/bin/go version | grep -q 'go#{GO_VERSION} '"
end

# login shell (ssh / bash -lc) の PATH に go を載せる。
file '/etc/profile.d/go.sh' do
  owner 'root'
  group 'root'
  mode '0644'
  content "export PATH=/usr/local/go/bin:$PATH\n"
end
