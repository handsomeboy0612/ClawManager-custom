#!/usr/bin/env bash
# join-node.sh — 将新服务器加入现有 K8s 集群（同机房扩容 Worker 节点）
#
# 用法一：交互方式（直接运行）
#   sudo bash join-node.sh
#
# 用法二：提供 join 命令方式（来自 install-region.sh 摘要或 kubeadm 输出）
#   sudo JOIN_CMD="kubeadm join 1.2.3.4:6443 --token xxx --discovery-token-ca-cert-hash sha256:yyy" \
#     bash join-node.sh
#
# JOIN_CMD 来自：
#   - install-region.sh 摘要文件（/root/clawmanager-region-*.txt）
#   - 或在 Master 上运行：kubeadm token create --print-join-command
# ──────────────────────────────────────────────────────────────────────────

set -euo pipefail

# ── 0. 权限检查 ─────────────────────────────────────────────────────────────

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo bash $0"
  exit 1
fi

# ── 1. 获取 join 命令 ────────────────────────────────────────────────────────

JOIN_CMD="${JOIN_CMD:-}"

if [[ -z "$JOIN_CMD" ]]; then
  echo "请输入 kubeadm join 命令（来自 install-region.sh 摘要或 Master 上的 kubeadm token create --print-join-command）："
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

echo "[K8s] 执行 join..."
eval "$JOIN_CMD"

# ── 5. 等待 kubelet 稳定 ─────────────────────────────────────────────────────

echo "[K8s] 等待 kubelet 稳定（最长 60s）..."
for i in $(seq 1 30); do
  if systemctl is-active --quiet kubelet; then
    echo "[K8s] kubelet 运行中 ✓"
    break
  fi
  sleep 2
  if [[ $i -eq 30 ]]; then
    echo "[K8s] kubelet 启动超时，请查看日志："
    echo "  journalctl -u kubelet -n 50 --no-pager"
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
