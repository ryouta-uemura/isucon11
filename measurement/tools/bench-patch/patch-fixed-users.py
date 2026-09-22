#!/usr/bin/env python3
"""ベンチのユーザー数を固定できるようにするパッチ。

score(N) の曲線を直接引くために使う。TTL 経由の増加では「何人になったか」を
制御できず、増加のたびに条件が変わってしまうため。

  BENCH_INIT_USERS=N  総ユーザー数を N にする（既定 7）
  BENCH_NO_ADD=1      userAdder を止めて N を固定する
"""
import sys

p = "scenario/load.go"
s = open(p).read()

# 1) os / strconv を import
old_imp = '''import (
	"context"
	"math/rand"
	"net/http"
	"strings"'''
new_imp = '''import (
	"context"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"'''
assert old_imp in s, "import ブロックが見つからない"
s = s.replace(old_imp, new_imp, 1)

# 2) 初期ユーザー数を環境変数から取る
old_add = '''	//通常ユーザー
	s.AddNormalUser(ctx, step, 6)
	s.AddIsuconUser(ctx, step)'''
new_add = '''	//通常ユーザー
	// BENCH_INIT_USERS で総ユーザー数を指定できるようにした（既定は従来どおり7人）。
	// AddIsuconUser で1人増えるので、AddNormalUser には N-1 を渡す。
	initNormalUsers := 6
	if v := os.Getenv("BENCH_INIT_USERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			initNormalUsers = n - 1
		}
	}
	s.AddNormalUser(ctx, step, initNormalUsers)
	s.AddIsuconUser(ctx, step)'''
assert old_add in s, "AddNormalUser の呼び出しが見つからない"
s = s.replace(old_add, new_add, 1)

# 3) userAdder を止められるようにする。
#    userAdderIsDropped は close しないこと。close すると loadViewer と
#    loadNormalUser がそれを「脱落シグナル」として受け取って走行が壊れる。
old_adder = '''func (s *Scenario) userAdder(ctx context.Context, step *isucandar.BenchmarkStep) {
	defer func() {
		close(userAdderIsDropped)'''
new_adder = '''func (s *Scenario) userAdder(ctx context.Context, step *isucandar.BenchmarkStep) {
	// BENCH_NO_ADD=1 でユーザー増加を止める。ユーザー数を固定して score(N) を
	// 測るための計測用スイッチ。userAdderIsDropped は close しない
	// （close すると viewer が脱落扱いになって走行が壊れる）。
	if os.Getenv("BENCH_NO_ADD") == "1" {
		<-ctx.Done()
		return
	}
	defer func() {
		close(userAdderIsDropped)'''
assert old_adder in s, "userAdder が見つからない"
s = s.replace(old_adder, new_adder, 1)

open(p, "w").write(s)
print("patched", p)
