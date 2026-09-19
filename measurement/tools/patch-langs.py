import re

LANGS = "nodejs|perl|php|python|ruby|rust"

# site.yml: langs.go 以外の langs ロールを外す
p = "site.yml"
s = open(p).read()
pat = re.compile(r"\n[ \t]*- langs\.(?:" + LANGS + r")(?=\n)")
n = len(pat.findall(s))
open(p, "w").write(pat.sub("", s))
print("site.yml: removed %d lang roles" % n)

# isucondition.yml: Go 以外のビルド include と service ファイル配置を外す
p = "roles/contestant/tasks/isucondition.yml"
s = open(p).read()
s = re.sub(r'- name: "[^"]*Include isucondition-(?:' + LANGS + r')\.yml"\n'
           r'[ \t]*include: isucondition-(?:' + LANGS + r')\.yml\n', "", s)
s = re.sub(r"\n[ \t]*- etc/systemd/system/isucondition\.(?:" + LANGS + r")\.service", "", s)
open(p, "w").write(s)

assert "isucondition-go.yml" in s, "go の include まで消してしまった"
assert "isucondition.go.service" in s, "go の service まで消してしまった"
for lang in LANGS.split("|"):
    assert lang not in s, lang + " が残っている"
print("isucondition.yml: go only")
