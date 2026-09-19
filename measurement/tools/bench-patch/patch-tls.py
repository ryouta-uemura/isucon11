#!/usr/bin/env python3
"""bench に -tls-skip-verify を足す。

ベンチを別VMに分けると、アプリVMの自己署名証明書をベンチVMが信頼する必要が出る。
証明書そのものを持ち回らずに済ませるため、ローカル練習用の opt-in フラグを用意する。
既定は false なので、付けなければ従来どおり検証する。

カレントディレクトリに main.go がある状態で実行する。冪等。
"""
import sys

p = "main.go"
s = open(p, encoding="utf-8").read()

if "tlsSkipVerify" in s:
    print("main.go: already patched (tls)")
    sys.exit(0)

old = """	promOut             string
	showVersion         bool
"""
new = """	promOut             string
	showVersion         bool
	tlsSkipVerify       bool
"""
assert old in s, "var ブロックが見つからない"
s = s.replace(old, new, 1)

old = """	flag.BoolVar(&showVersion, "version", false, "show version and exit 1")
"""
new = """	flag.BoolVar(&showVersion, "version", false, "show version and exit 1")
	flag.BoolVar(&tlsSkipVerify, "tls-skip-verify", false, "skip TLS certificate verification (local practice only)")
"""
assert old in s, "flag ブロックが見つからない"
s = s.replace(old, new, 1)

# flag.Parse() の後でないと値が入っていない
old = """	flag.Parse()

	// validate target
"""
new = """	flag.Parse()

	// ベンチを別ホストに置くと、対象の自己署名証明書を信頼する手段が要る。
	// 証明書を配布する代わりに、ローカル練習用に検証を切れるようにしておく。
	if tlsSkipVerify {
		agent.DefaultTLSConfig.InsecureSkipVerify = true
	}

	// validate target
"""
assert old in s, "flag.Parse() の直後が見つからない"
s = s.replace(old, new, 1)

open(p, "w", encoding="utf-8").write(s)
print("main.go: patched (tls-skip-verify)")
