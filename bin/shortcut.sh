cat << 'EOF' >> ~/.bashrc

# ISUCON 快適化エイリアス
alias a='sudo ./bin/analysis.sh'
alias c='sudo ./bin/log_clear'
alias b='./bin/bench'
alias run='sudo ./bin/run_all.sh'
EOF


echo "do source ~/.bashrc"
