#!/usr/bin/env bash
# install-region.sh — 一键部署新 ClawManager 机房（标准 K8s + kubeadm + ClawManager）
#
# 用法：
#   chmod +x install-region.sh && sudo ./install-region.sh <REGION_CODE>
#   示例：sudo ./install-region.sh cn-bj-1
#
# 前提：
#   - 全新 Ubuntu 20.04/22.04 服务器，root 权限
#   - 端口 30443 对 yunwu 服务器开放（ClawManager API）
#   - 端口 6443 对同机房其他节点开放（K8s API，扩容 Worker 时需要）
#
# 完成后输出「机房信息摘要」，把其中三项填入 yunwu 后台即可。
# ──────────────────────────────────────────────────────────────────────────

set -euo pipefail

# ── 0. 基础校验 ─────────────────────────────────────────────────────────────

REGION_CODE="${1:-}"
if [[ -z "$REGION_CODE" ]]; then
  echo "用法: sudo $0 <REGION_CODE>  (示例: cn-bj-1, xl-hk-1)"
  exit 1
fi

# REGION_CODE 必须：以小写字母开头，只能含小写字母/数字/连字符，长度 2-32
if [[ ! "$REGION_CODE" =~ ^[a-z][a-z0-9-]{1,31}$ ]]; then
  echo "[错误] REGION_CODE 格式不合法: '$REGION_CODE'"
  echo "  规则：以小写字母开头，仅含小写字母、数字和连字符（-），长度 2-32"
  echo "  示例：cn-bj-1 / us-lax-1 / xl-hk-1"
  exit 1
fi

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo $0 $REGION_CODE"
  exit 1
fi

# 端口预检：30443 (ClawManager NodePort) 和 6443 (K8s API) 不能被占用
check_port() {
  local port=$1 name=$2
  if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "(^|:)${port}\$"; then
    echo "[错误] 端口 ${port} (${name}) 已被占用，请先释放后重试"
    echo "  排查：ss -ltnp | grep ':${port}'"
    exit 1
  fi
}
# kubectl 已经能连上集群说明 K8s 已装好，跳过端口预检
if ! (command -v kubectl &>/dev/null && kubectl get node &>/dev/null 2>&1); then
  check_port 30443 "ClawManager NodePort"
  check_port 6443  "K8s API Server"
fi

# 失败时自动回滚：仅在脚本本次执行了 kubeadm init 时回滚
INSTALL_FRESH_K8S=0
SCRIPT_FAILED=1   # 默认认为失败，脚本走到末尾时清零
on_exit() {
  local code=$?
  if [[ $code -ne 0 && "$SCRIPT_FAILED" == "1" && "$INSTALL_FRESH_K8S" == "1" ]]; then
    echo ""
    echo "════════════════════════════════════════════════════════"
    echo "  [自动回滚] 脚本异常退出 (exit=$code)，正在执行 kubeadm reset..."
    echo "  如需保留现场调试，可 Ctrl+C 中断此处"
    echo "════════════════════════════════════════════════════════"
    sleep 5
    kubeadm reset -f --cri-socket=unix:///run/containerd/containerd.sock 2>/dev/null || true
    rm -rf /etc/cni/net.d /var/lib/etcd /var/lib/cni/networks /run/flannel 2>/dev/null || true
    # 删除网络接口残留，避免下次跑脚本时 flannel 拒绝配置 cni0
    ip link set cni0 down 2>/dev/null || true
    ip link delete cni0 2>/dev/null || true
    ip link set flannel.1 down 2>/dev/null || true
    ip link delete flannel.1 2>/dev/null || true
    iptables -F 2>/dev/null || true
    iptables -t nat -F 2>/dev/null || true
    echo "  [自动回滚] 完成。环境已恢复，可重新运行 install-region.sh"
  fi
}
trap on_exit EXIT

# ClawManager k8s YAML 地址（你 fork 的 raw 链接）
CLAWMANAGER_YAML_URL="https://raw.githubusercontent.com/handsomeboy0612/clawmanager-custom/main/deployments/k8s/clawmanager.yaml"

SERVER_IP=$(hostname -I | awk '{print $1}')
CLAWMANAGER_URL="https://${SERVER_IP}:30443"

# ── 1. 生成随机密钥 ──────────────────────────────────────────────────────────

gen_pass() { openssl rand -base64 24 | tr -d '/+=\n' | head -c "$1"; }

MYSQL_ROOT_PASS=$(gen_pass 24)
MYSQL_PASS=$(gen_pass 24)
JWT_SECRET=$(gen_pass 40)
EXTERNAL_API_KEY="claw-$(openssl rand -hex 24)"
ADMIN_INIT_PASS="Admin$(gen_pass 12)"
# MinIO 4 个独立密钥（root user/pass + access key/secret key），全随机
MINIO_ROOT_USER="claw$(gen_pass 12)"
MINIO_ROOT_PASS=$(gen_pass 24)
MINIO_ACCESS_KEY="claw$(gen_pass 12)"
MINIO_SECRET_KEY=$(gen_pass 32)

echo ""
echo "════════════════════════════════════════════════════════"
echo "  ClawManager 机房安装 — 区域代码: $REGION_CODE"
echo "  服务器 IP: $SERVER_IP"
echo "════════════════════════════════════════════════════════"
echo ""

# ── 2. 安装 K8s（kubeadm + containerd）──────────────────────────────────────

if command -v kubectl &>/dev/null && kubectl get node &>/dev/null 2>&1; then
  echo "[K8s] kubectl 已可用且集群正常，跳过安装"
else
  INSTALL_FRESH_K8S=1
  echo "[K8s] 安装 containerd + kubeadm..."

  # 清理可能的旧 K8s 网络残留（机器若曾是别集群的 Worker，必须清干净才能当新 Master）
  # - cni0 网桥可能仍带旧 pod 网段的 IP（如 10.244.1.1/24），导致 flannel 拒绝配置
  # - flannel.1 VTEP 接口同理
  # - IPAM 状态文件残留会让新 Pod 拿到旧网段的 IP，DNS 全废
  if ip link show cni0 &>/dev/null || ip link show flannel.1 &>/dev/null \
     || [[ -d /var/lib/cni/networks ]] || [[ -d /run/flannel ]]; then
    echo "[K8s] 检测到旧 K8s 网络残留，自动清理..."
    ip link set cni0 down 2>/dev/null || true
    ip link delete cni0  2>/dev/null || true
    ip link set flannel.1 down 2>/dev/null || true
    ip link delete flannel.1 2>/dev/null || true
    rm -rf /var/lib/cni/networks /run/flannel 2>/dev/null || true
    echo "[K8s] 旧网络残留清理完成 ✓"
  fi

  # 关闭 swap（K8s 要求）
  swapoff -a
  sed -i '/swap/s/^/#/' /etc/fstab

  # 内核模块
  modprobe overlay
  modprobe br_netfilter
  cat > /etc/modules-load.d/k8s.conf <<EOF
overlay
br_netfilter
EOF
  cat > /etc/sysctl.d/k8s.conf <<EOF
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
  sysctl --system

  # containerd
  apt-get update -qq
  apt-get install -y -qq containerd apt-transport-https ca-certificates curl gpg

  mkdir -p /etc/containerd
  containerd config default > /etc/containerd/config.toml
  sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
  systemctl restart containerd
  systemctl enable containerd

  # K8s apt 源
  K8S_VERSION="v1.29"
  mkdir -p /etc/apt/keyrings
  # --batch --yes: 无 tty 后台运行（CI、nohup、ansible）和 keyring 已存在时都能直通，不会卡交互
  rm -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  curl -fsSL "https://pkgs.k8s.io/core:/stable:/${K8S_VERSION}/deb/Release.key" \
    | gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] \
https://pkgs.k8s.io/core:/stable:/${K8S_VERSION}/deb/ /" \
    > /etc/apt/sources.list.d/kubernetes.list

  apt-get update -qq
  apt-get install -y -qq kubelet kubeadm kubectl
  apt-mark hold kubelet kubeadm kubectl

  # 初始化 Master
  echo "[K8s] kubeadm init（约 2-3 分钟）..."
  kubeadm init \
    --apiserver-advertise-address="${SERVER_IP}" \
    --pod-network-cidr=10.244.0.0/16 \
    --apiserver-cert-extra-sans="${SERVER_IP}" \
    | tee /root/kubeadm-init.log

  # 配置 kubectl
  mkdir -p /root/.kube
  cp /etc/kubernetes/admin.conf /root/.kube/config
  export KUBECONFIG=/root/.kube/config

  # 允许 Master 运行普通 Pod（单节点机房必须）
  kubectl taint nodes --all node-role.kubernetes.io/control-plane- || true

  # 安装 Flannel 网络插件
  echo "[K8s] 安装 Flannel 网络..."
  kubectl apply -f \
    https://raw.githubusercontent.com/flannel-io/flannel/master/Documentation/kube-flannel.yml

  echo "[K8s] 等待 Node Ready（最长 3 分钟，需 Flannel 镜像拉取完成）..."
  if ! kubectl wait --for=condition=Ready node --all --timeout=180s; then
    echo "[K8s] 等待 Node Ready 超时"
    echo "  排查命令："
    echo "    kubectl get nodes"
    echo "    kubectl -n kube-flannel get pods"
    echo "    journalctl -u kubelet -n 100 --no-pager"
    exit 1
  fi
  echo "[K8s] Node Ready ✓"
fi

export KUBECONFIG=/root/.kube/config

# ── 3. 创建 hostPath 数据目录 ────────────────────────────────────────────────

# k8s/clawmanager.yaml 中 PV 使用 hostPath，需要目录预先存在
echo "[准备] 创建 PV 数据目录..."
mkdir -p /tmp/clawmanager/system/mysql
mkdir -p /tmp/clawmanager/system/minio

# ── 4. 下载并渲染 ClawManager YAML ──────────────────────────────────────────

echo "[ClawManager] 下载 YAML..."
TMP_YAML=$(mktemp /tmp/clawmanager-XXXXXX.yaml)
curl -sSfL "$CLAWMANAGER_YAML_URL" -o "$TMP_YAML"

# 替换 Secret 中的默认密码占位符（MySQL/JWT 走值替换，MinIO 走 key 精准替换避免误伤）
sed -i \
  -e "s|root123|${MYSQL_ROOT_PASS}|g" \
  -e "s|clawreef123|${MYSQL_PASS}|g" \
  -e "s|change-me-in-production|${JWT_SECRET}|g" \
  -e "s|minio-root-user: minioadmin|minio-root-user: ${MINIO_ROOT_USER}|" \
  -e "s|minio-root-password: minioadmin123|minio-root-password: ${MINIO_ROOT_PASS}|" \
  -e "s|minio-access-key: minioadmin|minio-access-key: ${MINIO_ACCESS_KEY}|" \
  -e "s|minio-secret-key: minioadmin123|minio-secret-key: ${MINIO_SECRET_KEY}|" \
  "$TMP_YAML"

# minio 版本策略：yaml 已 pin 到经 Docker Hub 校验存在的稳定 tag（RELEASE.2025-09-07T16-13-09Z）。
# 为保证版本可复现，这里不再在部署前强制把 RELEASE.* 替换为 latest；
# 若该 tag 未来不可拉取，下方"等待 minio 就绪"阶段会自动 fallback 到 latest 作为安全网。

echo "[ClawManager] 应用 YAML..."
kubectl apply -f "$TMP_YAML"
rm -f "$TMP_YAML"

# ── 5. 注入 CLAWMANAGER_EXTERNAL_API_KEY ────────────────────────────────────

echo "[ClawManager] 注入 External API Key..."
for i in $(seq 1 30); do
  if kubectl get deployment clawmanager-app -n clawmanager-system &>/dev/null 2>&1; then
    break
  fi
  sleep 2
done

kubectl set env deployment/clawmanager-app \
  CLAWMANAGER_EXTERNAL_API_KEY="${EXTERNAL_API_KEY}" \
  -n clawmanager-system

# ── 6. 等待所有 Pod 就绪 ─────────────────────────────────────────────────────

echo "[ClawManager] 等待 Pod 就绪（最长 10 分钟，首次需拉取镜像）..."
kubectl rollout status deployment/mysql           -n clawmanager-system --timeout=600s
kubectl rollout status deployment/clawmanager-app -n clawmanager-system --timeout=600s
kubectl rollout status deployment/skill-scanner   -n clawmanager-system --timeout=300s

# minio 是上传文件依赖，必须 wait；若 yaml tag 不可用则自动 fallback 到 latest 再 wait 一次
echo "[ClawManager] 等待 minio 就绪..."
if ! kubectl rollout status deployment/minio -n clawmanager-system --timeout=180s 2>/dev/null; then
  echo "  [警告] minio 启动失败（多半是镜像 tag 不可用），自动 fallback 到 minio/minio:latest..."
  kubectl set image deployment/minio -n clawmanager-system minio=minio/minio:latest
  kubectl rollout status deployment/minio -n clawmanager-system --timeout=300s
fi

echo "[ClawManager] 所有 Pod 就绪 ✓"

# ── 7. 修改 ClawManager 管理员默认密码 ───────────────────────────────────────

echo "[ClawManager] 初始化管理员密码..."
HTTP_CODE="000"
for i in $(seq 1 20); do
  HTTP_CODE=$(curl -sk -o /dev/null -w "%{http_code}" \
    -X POST "https://localhost:30443/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"admin123"}') || true
  if [[ "$HTTP_CODE" == "200" ]]; then break; fi
  sleep 5
done

if [[ "$HTTP_CODE" == "200" ]]; then
  TOKEN=$(curl -sk \
    -X POST "https://localhost:30443/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"admin","password":"admin123"}' \
    | grep -o '"access_token":"[^"]*"' | cut -d'"' -f4) || true

  if [[ -n "${TOKEN:-}" ]]; then
    curl -sk -o /dev/null \
      -X PUT "https://localhost:30443/api/v1/users/1" \
      -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" \
      -d "{\"password\":\"${ADMIN_INIT_PASS}\"}" || true
    echo "  [ClawManager] 管理员密码已更新 ✓"
  fi
else
  ADMIN_INIT_PASS="admin123（默认，请登录后立即修改！）"
  echo "  [警告] 无法自动修改密码，使用默认 admin123"
fi

# ── 8. 生成 kubeadm join 命令（供 join-node.sh 使用）────────────────────────

JOIN_CMD=""
JOIN_CMD=$(kubeadm token create --print-join-command 2>/dev/null || true)
if [[ -z "$JOIN_CMD" ]]; then
  echo "  [警告] 无法生成 join 命令，扩容时请在本机重新运行：kubeadm token create --print-join-command"
fi

# ── 9. 输出机房信息摘要 ───────────────────────────────────────────────────────

SUMMARY_FILE="/root/clawmanager-region-${REGION_CODE}.txt"
cat > "$SUMMARY_FILE" <<EOF
════════════════════════════════════════════════════════════════
  ClawManager 机房安装摘要 — ${REGION_CODE}
  安装时间: $(date '+%Y-%m-%d %H:%M:%S %Z')
════════════════════════════════════════════════════════════════

【yunwu 机房管理后台填写内容（三项必填）】
  机房代码 (Code):              ${REGION_CODE}
  ClawManager Base URL:         ${CLAWMANAGER_URL}
  ClawManager External API Key: ${EXTERNAL_API_KEY}

【ClawManager 控制台登录】
  地址:   ${CLAWMANAGER_URL}
  用户名: admin
  密码:   ${ADMIN_INIT_PASS}

【添加 Worker 节点（同机房扩容）】
  使用 join-node.sh 脚本，或直接在新节点上运行：
$(if [[ -n "$JOIN_CMD" ]]; then echo "  $JOIN_CMD"; else echo "  kubeadm token create --print-join-command  # 在本机运行获取最新 join 命令"; fi)

【密钥备份（请妥善保管）】
  MySQL Root Password:  ${MYSQL_ROOT_PASS}
  MySQL User Password:  ${MYSQL_PASS}
  JWT Secret:           ${JWT_SECRET}
  MinIO Root User:      ${MINIO_ROOT_USER}
  MinIO Root Password:  ${MINIO_ROOT_PASS}
  MinIO Access Key:     ${MINIO_ACCESS_KEY}
  MinIO Secret Key:     ${MINIO_SECRET_KEY}
  K8s Admin kubeconfig: /root/.kube/config

【完全重装本机房（一键清场）】
  kubeadm reset -f --cri-socket=unix:///run/containerd/containerd.sock
  rm -rf /etc/cni/net.d /etc/kubernetes /var/lib/etcd /var/lib/kubelet \\
         /tmp/clawreef /tmp/clawmanager /var/lib/cni/networks /run/flannel /root/.kube
  ip link set cni0 down 2>/dev/null; ip link delete cni0 2>/dev/null
  ip link set flannel.1 down 2>/dev/null; ip link delete flannel.1 2>/dev/null
  iptables -F && iptables -t nat -F
  # 然后重新跑：sudo ./install-region.sh ${REGION_CODE}
════════════════════════════════════════════════════════════════
EOF

cat "$SUMMARY_FILE"
echo ""
echo "摘要已保存到: $SUMMARY_FILE"
echo ""
echo "下一步：将【yunwu 机房管理后台填写内容】的三项复制到"
echo "  yunwu 后台 → OpenClaw 机房管理 → 新建机房"

# 走到这里说明全部成功，清除回滚标记
SCRIPT_FAILED=0
