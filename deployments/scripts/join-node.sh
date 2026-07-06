#!/usr/bin/env bash
# join-node.sh — 将新服务器加入现有 K8s 集群（同机房扩容 Worker 节点）
#
# 用法一：自动从 Master 拉 join 命令（推荐）
#   sudo bash join-node.sh --from-master root@1.2.3.4
#   前提：本机已能 SSH 免密登录 Master（ssh-keygen + ssh-copy-id）
#
# 用法二：通过环境变量传 JOIN_CMD
#   sudo JOIN_CMD="kubeadm join 1.2.3.4:6443 --token xxx --discovery-token-ca-cert-hash sha256:yyy" \
#     bash join-node.sh
#
# 用法三：交互方式（直接运行，回车后粘贴）
#   sudo bash join-node.sh
#
# JOIN_CMD 来自：
#   - install-region.sh 摘要文件（/root/clawmanager-region-*.txt）
#   - 或在 Master 上运行：kubeadm token create --print-join-command
# ──────────────────────────────────────────────────────────────────────────

set -euo pipefail

# ── 0. 权限检查 ─────────────────────────────────────────────────────────────

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo bash $0 $*"
  exit 1
fi

# ── 1. 获取 join 命令 ────────────────────────────────────────────────────────

JOIN_CMD="${JOIN_CMD:-}"
FROM_MASTER=""

# 解析 --from-master 参数
while [[ $# -gt 0 ]]; do
  case "$1" in
    --from-master)
      FROM_MASTER="${2:-}"
      shift 2
      ;;
    --from-master=*)
      FROM_MASTER="${1#*=}"
      shift
      ;;
    -h|--help)
      grep '^#' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "[警告] 忽略未知参数: $1"
      shift
      ;;
  esac
done

# 模式 A：从 Master 自动拉 token
if [[ -z "$JOIN_CMD" && -n "$FROM_MASTER" ]]; then
  echo "[token] 通过 SSH 从 Master ($FROM_MASTER) 自动获取 join 命令..."
  if ! command -v ssh &>/dev/null; then
    apt-get update -qq && apt-get install -y -qq openssh-client
  fi
  JOIN_CMD=$(ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
    -o ConnectTimeout=10 \
    "$FROM_MASTER" "kubeadm token create --print-join-command" 2>&1) || {
    echo "[错误] 从 Master 拉取 join 命令失败"
    echo "  返回: $JOIN_CMD"
    echo "  排查："
    echo "    1) 是否已配置 SSH 免密？"
    echo "       ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519"
    echo "       ssh-copy-id $FROM_MASTER"
    echo "    2) Master 上 kubeadm 是否可执行？(需 root)"
    echo "       ssh $FROM_MASTER 'kubeadm version'"
    exit 1
  }
  echo "[token] 已获取 join 命令 ✓"
fi

# 模式 B/C：交互输入
if [[ -z "$JOIN_CMD" ]]; then
  echo "请输入 kubeadm join 命令（或用 --from-master root@<master-ip> 自动获取）："
  echo "示例：kubeadm join 1.2.3.4:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:xxx"
  echo ""
  read -rp "join 命令: " JOIN_CMD
fi

if [[ -z "$JOIN_CMD" ]]; then
  echo "join 命令不能为空"
  exit 1
fi

# 从 join 命令中解析 master IP（用于预检）
MASTER_ADDR=$(echo "$JOIN_CMD" | grep -oP '(?<=join )[^\s]+' || true)

echo ""
echo "════════════════════════════════════════════════════════"
echo "  加入 K8s 集群"
echo "  Master: ${MASTER_ADDR:-（解析失败，继续执行）}"
echo "  本机 IP: $(hostname -I | awk '{print $1}')"
echo "════════════════════════════════════════════════════════"
echo ""

# ── 2. 安装前置依赖（containerd + kubeadm + kubelet）─────────────────────────

if command -v kubeadm &>/dev/null; then
  echo "[K8s] kubeadm 已安装，跳过安装步骤"
else
  echo "[K8s] 安装 containerd + kubeadm + kubelet..."

  swapoff -a
  sed -i '/swap/s/^/#/' /etc/fstab

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

  apt-get update -qq
  apt-get install -y -qq containerd apt-transport-https ca-certificates curl gpg

  mkdir -p /etc/containerd
  containerd config default > /etc/containerd/config.toml
  sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
  systemctl restart containerd
  systemctl enable containerd

  K8S_VERSION="v1.29"
  mkdir -p /etc/apt/keyrings
  curl -fsSL "https://pkgs.k8s.io/core:/stable:/${K8S_VERSION}/deb/Release.key" \
    | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] \
https://pkgs.k8s.io/core:/stable:/${K8S_VERSION}/deb/ /" \
    > /etc/apt/sources.list.d/kubernetes.list

  apt-get update -qq
  apt-get install -y -qq kubelet kubeadm kubectl
  apt-mark hold kubelet kubeadm kubectl

  echo "[K8s] 依赖安装完成 ✓"
fi

# ── 3. 连通性预检 ───────────────────────────────────────────────────────────

if [[ -n "${MASTER_ADDR:-}" ]]; then
  MASTER_IP="${MASTER_ADDR%%:*}"
  MASTER_PORT="${MASTER_ADDR##*:}"
  echo "[预检] 测试与 Master ${MASTER_IP}:${MASTER_PORT} 的连通性..."
  if ! curl -sk --connect-timeout 5 "https://${MASTER_IP}:${MASTER_PORT}/version" -o /dev/null; then
    echo "  [警告] 无法访问 Master API，请确认："
    echo "    1. Master 防火墙是否开放 ${MASTER_PORT} 端口"
    echo "    2. IP 是否正确"
    read -rp "  仍然继续？[y/N] " CONFIRM
    [[ "${CONFIRM:-N}" == "y" || "${CONFIRM:-N}" == "Y" ]] || exit 1
  else
    echo "  [预检] Master 可达 ✓"
  fi
fi

# ── 4. 执行 join ─────────────────────────────────────────────────────────────

# 诊断函数：失败时统一输出 kubelet/containerd 关键信息
dump_diagnostics() {
  echo ""
  echo "════════════════════════════════════════════════════════"
  echo "  [诊断] 关键服务状态"
  echo "════════════════════════════════════════════════════════"
  echo "── systemctl status kubelet ──"
  systemctl status kubelet --no-pager -l 2>&1 | head -30 || true
  echo ""
  echo "── systemctl status containerd ──"
  systemctl status containerd --no-pager -l 2>&1 | head -20 || true
  echo ""
  echo "── journalctl -u kubelet -n 60 ──"
  journalctl -u kubelet -n 60 --no-pager 2>&1 || true
  echo ""
  echo "── 常见排查方向 ──"
  echo "  1) Master 6443 防火墙：从本机 curl -sk https://${MASTER_ADDR:-MASTER_IP:6443}/version"
  echo "  2) token 已过期：在 Master 上重新 kubeadm token create --print-join-command"
  echo "  3) 时钟不同步：date  vs Master 上的 date  （差距 >5s 需 ntpdate 校正）"
  echo "  4) swap 没关：free -h | grep -i swap  应全为 0"
  echo "  5) cgroup driver 不一致：cat /etc/containerd/config.toml | grep SystemdCgroup"
  echo "════════════════════════════════════════════════════════"
}

echo "[K8s] 执行 join..."
if ! eval "$JOIN_CMD"; then
  echo "[错误] kubeadm join 执行失败"
  dump_diagnostics
  exit 1
fi

# ── 5. 等待 kubelet 稳定 ─────────────────────────────────────────────────────

echo "[K8s] 等待 kubelet 稳定（最长 60s）..."
for i in $(seq 1 30); do
  if systemctl is-active --quiet kubelet; then
    echo "[K8s] kubelet 运行中 ✓"
    break
  fi
  sleep 2
  if [[ $i -eq 30 ]]; then
    echo "[错误] kubelet 启动超时"
    dump_diagnostics
    exit 1
  fi
done

# ── 6. 完成提示 ─────────────────────────────────────────────────────────────

echo ""
echo "════════════════════════════════════════════════════════"
echo "  Worker 节点加入完成！"
echo ""
echo "  请在 Master 节点上运行以下命令确认（约 1-2 分钟变 Ready）："
echo "    kubectl get nodes"
echo ""
echo "  ⚠️  hostPath PV 注意："
echo "    OpenClaw 数据 PVC 使用 hostPath 存储，数据在创建 Pod 时"
echo "    所在的节点本地。新节点加入后，新实例可能被调度到此节点，"
echo "    但历史数据仍在原节点。"
echo ""
echo "  建议在 Master 上给 OpenClaw 实例加 nodeSelector 锁定调度："
echo "    kubectl label node <master-node-name> clawmanager/role=primary"
echo "    （需同步修改 ClawManager pod_service.go 中的 NodeSelector）"
echo "════════════════════════════════════════════════════════"
