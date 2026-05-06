# ClawManager 机房部署手册

## 目录

- [架构说明](#架构说明)
- [新机房一键安装](#新机房一键安装-install-regionsh)
- [同机房扩容 Worker 节点](#同机房扩容-worker-节点-join-nodesh)
- [机房状态管理](#机房状态管理)
- [常见问题](#常见问题)

---

## 架构说明

```
yunwu.ai（控制面）
    │
    ├─── 机房 A（cn-bj-1）
    │        K8s Master + ClawManager
    │        NodePort :30443 ← yunwu 通过此端口调度
    │        Worker 节点 1（可选扩容）
    │        Worker 节点 2（可选扩容）
    │
    └─── 机房 B（us-lax-1）
             K8s Master + ClawManager
             NodePort :30443
```

- **每个机房**：独立 K8s 集群 + 独立 ClawManager 实例 + 独立 MySQL
- **yunwu** 通过 `openclaw_regions` 表记录每个机房的 URL 和 API Key，按优先级和容量自动调度
- **实例强绑定**：一旦创建，实例的机房不变；编辑/重启/删除都走原机房

---

## 新机房一键安装（install-region.sh）

### 前提条件

| 项目 | 要求 |
|---|---|
| 操作系统 | Ubuntu 20.04/22.04 或 Debian 11/12 |
| 权限 | root |
| 端口（对外） | **30443**（yunwu 访问 ClawManager API） |
| 端口（内部） | 6443、8472/UDP、10250（K3s 集群内节点互通） |
| 网络 | 可访问 GitHub（或配置镜像，见下文） |

### 安装步骤

```bash
# 1. 下载脚本
curl -sSL https://raw.githubusercontent.com/handsomeboy0612/clawmanager-custom/main/deployments/scripts/install-region.sh \
  -o install-region.sh
chmod +x install-region.sh

# 2. 执行安装（替换 cn-bj-1 为你的机房代码）
sudo ./install-region.sh cn-bj-1
```

脚本会自动安装：`containerd` → `kubeadm/kubelet/kubectl` → K8s Master（`kubeadm init`）→ Flannel 网络 → ClawManager。

**国内服务器**（访问 GitHub 慢）：脚本内 kubeadm 使用阿里云镜像加速——在脚本 K8s apt 源那段改用国内源：
```bash
# 将 pkgs.k8s.io 替换为阿里云镜像
https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.29/deb/
```

### 安装耗时

| 阶段 | 预计时长 |
|---|---|
| K3s 安装 | 1–3 分钟 |
| 镜像拉取（ClawManager + MySQL） | 3–10 分钟（取决于网速） |
| Pod 就绪 | 1–2 分钟 |
| **合计** | **5–15 分钟** |

### 安装完成后

脚本输出「机房信息摘要」并保存到 `/root/clawmanager-region-<CODE>.txt`，内容示例：

```
【yunwu 机房管理后台填写内容】
  机房代码 (Code):              cn-bj-1
  ClawManager Base URL:         https://1.2.3.4:30443
  ClawManager External API Key: claw-abcdef...
```

进入 **yunwu 后台 → OpenClaw 机房管理 → 新建机房**，填入上述三项，点「测试连接」成功后保存。

---

## 同机房扩容 Worker 节点（join-node.sh）

当现有机房资源不足时，向同一个 K3s 集群添加新 Worker 节点。

### 什么时候需要扩容

- OpenClaw 实例因资源不足无法调度（Pod Pending）
- 服务器 CPU/内存使用率持续 >80%

### 操作步骤

**第一步：在 Master 节点获取 join 命令**

```bash
# SSH 到 Master 节点，生成有效的 join 命令（token 有效期 24h）
kubeadm token create --print-join-command
# 输出示例：
# kubeadm join 1.2.3.4:6443 --token abcdef.0123456789abcdef \
#   --discovery-token-ca-cert-hash sha256:xxxxxxxx...

# 或查看 install-region.sh 生成的摘要（已包含安装时的 join 命令）
cat /root/clawmanager-region-<CODE>.txt
```

**第二步：在新服务器上执行 join**

```bash
# 方式 A：脚本方式（推荐，自动安装依赖）
curl -sSL https://raw.githubusercontent.com/handsomeboy0612/clawmanager-custom/main/deployments/scripts/join-node.sh \
  -o join-node.sh
chmod +x join-node.sh
sudo JOIN_CMD="kubeadm join 1.2.3.4:6443 --token xxx --discovery-token-ca-cert-hash sha256:yyy" \
  ./join-node.sh

# 方式 B：手动方式（Master 上已有 kubeadm，Worker 上也已安装好）
kubeadm join 1.2.3.4:6443 --token xxx --discovery-token-ca-cert-hash sha256:yyy
```

**第三步：在 Master 上确认**

```bash
kubectl get nodes
# 等待新节点状态变为 Ready（约 1-2 分钟）
```

### ⚠️ hostPath 存储注意事项

当前 ClawManager 使用 `local-path` StorageClass，PVC 数据存在**创建 Pod 的节点**本地。

扩容 Worker 节点后，新实例可能被调度到新节点，但数据目录在 Master：

```
Master:  /var/lib/rancher/k3s/storage/pvc-xxx/  ← 老实例数据在这里
Worker:  （空，无数据）                           ← 新实例可能调度到这
```

**解决方案（二选一）：**

| 方案 | 适用场景 | 操作 |
|---|---|---|
| 固定调度到 Master（推荐，改动小） | 单台物理机或主力机性能充足 | 见下方 |
| 改用 NFS / Longhorn 共享存储 | 多节点均衡调度，数据高可用 | 较复杂，按需实施 |

**固定调度到 Master（推荐）：**

```bash
# 给 Master 打标签
kubectl label node <master-node-name> clawmanager/role=primary

# 修改 ClawManager 创建 OpenClaw Pod 的 nodeSelector
# 在 pod_service.go 的 PodSpec 里加：
# NodeSelector: map[string]string{"clawmanager/role": "primary"}
```

> 这样 OpenClaw 数据 Pod 始终调度到 Master，Worker 节点只用于跑其他无状态工作负载。

---

## 机房状态管理

| 状态 | 含义 | 新建实例 | 已有实例 |
|---|---|---|---|
| **active** | 正常运行 | ✅ 接单 | ✅ 正常 |
| **paused** | 维护中，不接新单 | ❌ 拒绝 | ✅ 正常 |
| **unhealthy** | 健康检查失败（自动切入） | ❌ 拒绝 | ❌ 拒绝 |
| **disabled** | 人工完全停用 | ❌ 拒绝 | ❌ 拒绝 |

### 机房维护流程

```
正常运营
  → 计划维护前：改为 paused（已有用户不受影响，不再分配新实例）
  → 维护完成：改回 active

机房退役
  → 改为 paused → 通知用户迁移 → 等实例归零 → 改为 disabled → 删除记录
```

---

## 常见问题

### Q: 测试连接报错"无法访问 ClawManager"

检查顺序：
1. `curl -sk https://<IP>:30443/api/v1/instances` 在 yunwu 服务器上能否执行
2. 防火墙是否放行了 30443 端口：`ufw status` 或 `iptables -L`
3. ClawManager Pod 是否正常：`kubectl get pods -n clawmanager-system`

### Q: 测试连接报"API Key 无效"

ClawManager 的 `CLAWMANAGER_EXTERNAL_API_KEY` 环境变量与 yunwu 填入的 Key 不一致，确认：
```bash
kubectl get deployment clawmanager-app -n clawmanager-system \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CLAWMANAGER_EXTERNAL_API_KEY")].value}'
```

### Q: 安装脚本卡在拉取镜像

```bash
# 查看拉取进度
kubectl get pods -n clawmanager-system
kubectl describe pod <pod-name> -n clawmanager-system | tail -20
```

国内服务器建议配置 containerd 镜像加速：
```bash
mkdir -p /etc/containerd
cat >> /etc/containerd/config.toml <<EOF

[plugins."io.containerd.grpc.v1.cri".registry.mirrors."ghcr.io"]
  endpoint = ["https://ghcr.nju.edu.cn"]
EOF
systemctl restart containerd
```

### Q: 新节点 join 后 OpenClaw Pod 调度失败

参考[固定调度到 Master](#️-hostpath-存储注意事项)方案。
