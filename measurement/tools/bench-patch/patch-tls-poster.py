#!/usr/bin/env python3
"""-tls-skip-verify をポスターにも効かせる。

bench のポスター(scenario/posting.go)は agent.DefaultTLSConfig を使わず、
自前の http.Transport + tls.Config を組んでいる。そのため main.go 側で
InsecureSkipVerify を立てても POST /api/condition だけが証明書検証で失敗し、
「エラーは出ないのにコンディションが1件も届かない」状態になる。

bench ディレクトリ直下で実行する。冪等。
"""
import sys

# --- scenario/posting.go ---------------------------------------------------
p = "scenario/posting.go"
s = open(p, encoding="utf-8").read()

if "TLSSkipVerify" in s:
    print("posting.go: already patched")
else:
    old = """var (
	targetBaseURLMapMutex sync.Mutex
	targetBaseURLMap      = map[string]string{}
)"""
    new = """var (
	targetBaseURLMapMutex sync.Mutex
	targetBaseURLMap      = map[string]string{}
)

// TLSSkipVerify はポスターの TLS 証明書検証を切る。main の -tls-skip-verify から設定する。
// ベンチを対象と別ホストに置いたときに、自己署名証明書を配布せずに済ませるため。
var TLSSkipVerify bool"""
    assert old in s, "posting.go の var ブロックが見つからない"
    s = s.replace(old, new, 1)

    old = """		TLSClientConfig: &tls.Config{
			ServerName: fqdn,
		},"""
    new = """		TLSClientConfig: &tls.Config{
			ServerName:         fqdn,
			InsecureSkipVerify: TLSSkipVerify,
		},"""
    assert old in s, "keepPosting の TLSClientConfig が見つからない"
    s = s.replace(old, new, 1)

    old = """		TLSClientConfig:   &tls.Config{},"""
    new = """		TLSClientConfig:   &tls.Config{InsecureSkipVerify: TLSSkipVerify},"""
    assert old in s, "2つ目の TLSClientConfig が見つからない"
    s = s.replace(old, new, 1)

    open(p, "w", encoding="utf-8").write(s)
    print("posting.go: patched")

# --- main.go ---------------------------------------------------------------
p = "main.go"
s = open(p, encoding="utf-8").read()

if "scenario.TLSSkipVerify" in s:
    print("main.go: already wired")
    sys.exit(0)

old = """	if tlsSkipVerify {
		agent.DefaultTLSConfig.InsecureSkipVerify = true
	}"""
new = """	if tlsSkipVerify {
		agent.DefaultTLSConfig.InsecureSkipVerify = true
		// ポスターは agent.DefaultTLSConfig を使わず自前の Transport を持つので別途設定する
		scenario.TLSSkipVerify = true
	}"""
assert old in s, "main.go の tls-skip-verify 適用箇所が見つからない（先に patch-tls.py を当てる）"
s = s.replace(old, new, 1)
open(p, "w", encoding="utf-8").write(s)
print("main.go: wired scenario.TLSSkipVerify")
