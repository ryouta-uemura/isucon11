#!/usr/bin/env python3
"""bench/main.go に -score-dump フラグと1秒ごとのスコア出力を足す。

カレントディレクトリに main.go がある状態で実行する。冪等（適用済みなら何もしない）。
"""
import sys

p = "main.go"
s = open(p, encoding="utf-8").read()

if "scoreDumpPath" in s:
    print("main.go: already patched")
    sys.exit(0)

# 1) 変数を足す
old = """	promOut             string
	showVersion         bool
"""
new = """	promOut             string
	showVersion         bool
	scoreDumpPath       string
"""
assert old in s, "var ブロックが見つからない"
s = s.replace(old, new, 1)

# 2) フラグを足す
old = """	flag.BoolVar(&showVersion, "version", false, "show version and exit 1")
"""
new = """	flag.BoolVar(&showVersion, "version", false, "show version and exit 1")
	flag.StringVar(&scoreDumpPath, "score-dump", "", "path to write per-second score snapshots as JSONL")
"""
assert old in s, "flag ブロックが見つからない"
s = s.replace(old, new, 1)

# 3) main() の先頭で出力先を開く
old = """	if showVersion {
		os.Exit(1)
	}
"""
new = """	if showVersion {
		os.Exit(1)
	}

	openScoreDump(scoreDumpPath)
	defer closeScoreDump()
"""
assert old in s, "main() の先頭が見つからない"
s = s.replace(old, new, 1)

# 4) 途中経過ループを 1 秒刻みにして、スコアを毎秒書き出す。
#    ポータルへの送信(sendResult)は元どおり3秒間隔、
#    AdminLogger へのスコア出力も元どおり15秒間隔を保つ。
old = """		count := 0
		for {
			// 途中経過を3秒毎に送信
			timer := time.After(3 * time.Second)
			sendResult(s, step.Result(), false, count%5 == 0)
"""
new = """		count := 0
		for {
			// スコアのスナップショットを1秒刻みで取りたいのでループ自体を1秒にし、
			// sendResult は3回に1回だけ呼ぶ（送信頻度は元の3秒間隔のまま）。
			// AdminLogger へのスコア出力も元と同じ15秒間隔になる。
			timer := time.After(1 * time.Second)
			if count%3 == 0 {
				sendResult(s, step.Result(), false, count%15 == 0)
			}
			dumpScore(step.Result())
"""
assert old in s, "途中経過ループが見つからない"
s = s.replace(old, new, 1)

open(p, "w", encoding="utf-8").write(s)
print("main.go: patched")
