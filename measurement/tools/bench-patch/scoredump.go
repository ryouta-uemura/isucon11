package main

// 走行中のスコアを1秒ごとに JSONL へ書き出す（レイヤ2）。
//
// ベンチは終了時にしかスコア内訳を出さないため、「レイテンシが悪化した秒」と
// 「加点が止まった秒」を突き合わせられない。ここでは nginx のアクセスログと
// 同じ時間軸（epoch秒）でスナップショットを残し、analyze.py 側で重ねる。
//
// スコアの算出は main.go の sendResult と同じ手順を踏む。ズレると意味がないので、
// critical/timeout/deduction の優先順位も含めて向こうに合わせてある。

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/isucon/isucandar"
	"github.com/isucon/isucon11-qualify/bench/scenario"
)

type scoreSnapshot struct {
	TS        float64          `json:"ts"`        // epoch秒。access.log の time と同じ軸
	Raw       int64            `json:"raw"`       // 減点前の素点
	Deduction int64            `json:"deduction"` // 減点合計(タイムアウト/10 を含む)
	Timeout   int64            `json:"timeout"`   // タイムアウト数
	Score     int64            `json:"score"`     // raw - deduction
	Tags      map[string]int64 `json:"tags"`      // タグ別の加点回数
}

var scoreDumpFile *os.File

func openScoreDump(path string) {
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	scoreDumpFile = f
}

func closeScoreDump() {
	if scoreDumpFile != nil {
		scoreDumpFile.Close()
		scoreDumpFile = nil
	}
}

func dumpScore(result *isucandar.BenchmarkResult) {
	if scoreDumpFile == nil {
		return
	}

	raw := result.Score.Sum()

	// sendResult と同じ分岐・同じ優先順位で数える
	var deduction, timeoutCount int64
	for _, err := range result.Errors.All() {
		isCritical, isTimeout, isDeduction := checkError(err)
		switch true {
		case isCritical:
			// critical は pass/fail を左右するがスコア計算には入らない
		case isTimeout:
			timeoutCount++
		case isDeduction:
			if scenario.IsValidation(err) {
				deduction += 50
			} else {
				deduction++
			}
		}
	}
	deductionTotal := deduction + timeoutCount/10

	// SetScoreTags で未出現のタグも 0 埋めしておく（列が途中から生えるのを防ぐ）
	table := result.Score.Breakdown()
	scenario.SetScoreTags(table)
	tags := make(map[string]int64, len(table))
	for tag, count := range table {
		tags[strings.TrimRight(string(tag), " ")] = count
	}

	b, err := json.Marshal(scoreSnapshot{
		TS:        float64(time.Now().UnixNano()) / 1e9,
		Raw:       raw,
		Deduction: deductionTotal,
		Timeout:   timeoutCount,
		Score:     raw - deductionTotal,
		Tags:      tags,
	})
	if err != nil {
		return
	}
	// 途中でベンチが落ちても読めるよう都度 flush する
	scoreDumpFile.Write(append(b, '\n'))
	scoreDumpFile.Sync()
}
