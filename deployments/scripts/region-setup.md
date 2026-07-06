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
| 端口（内部） | 6443、8472/UDP、10250（K8s 集群内节点互通） |
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

脚本特性：
- REGION_CODE 格式校验（`^[a-z][a-z0-9-]{1,31}$`）
- 端口预检（30443 / 6443 已占用立即报错）
- 失败自动回滚（`kubeadm reset`，5 秒内可 Ctrl+C 跳过）
- MySQL / JWT / MinIO（4 项）密钥全随机化
- 摘要写入 `/root/clawmanager-region-<CODE>.txt`，含 join 命令

### 安装耗时

| 阶段 | 预计时长 |
|---|---|
| K8s 安装（kubeadm + Flannel） | 2–5 分钟 |
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

当现有机房资源不足时，向同一个 K8s 集群添加新 Worker 节点。

### 什么时候需要扩容

- OpenClaw 实例因资源不足无法调度（Pod Pending）
- 服务器 CPU/内存使用率持续 >80%

### 操作步骤

**【推荐】方式 A：从 Master 自动拉 token（一行搞定）**

前提：在新 Worker 上能 SSH 免密登录 Master。

```bash
# 1) 一次性配置 SSH 免密（在新 Worker 上执行）
ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519   # 已有可跳过
ssh-copy-id root@<master-ip>                            # 输入一次 master 密码

# 2) 下载 join-node.sh 并执行
curl -sSL https://raw.githubusercontent.com/handsomeboy0612/clawmanager-custom/main/deployments/scripts/join-node.sh \
  -o join-node.sh
chmod +x join-node.sh

sudo bash join-node.sh --from-master root@<master-ip>
```

脚本会自动 SSH 到 Master 跑 `kubeadm token create --print-join-command`、安装依赖、执行 join、做诊断输出。失败会自动 dump kubelet/containerd 日志。

**方式 B：手动粘贴 join 命令（无 SSH 免密时）**

```bash
# Master 上获取 join 命令
ssh root@<master-ip> 'kubeadm token create --print-join-command'

# 在新 Worker 上：
sudo JOIN_CMD="kubeadm join 1.2.3.4:6443 --token xxx --discovery-token-ca-cert-hash sha256:yyy" \
  bash join-node.sh

# 或交互方式（直接运行后粘贴）：
sudo bash join-node.sh
```

**最后：在 Master 上确认**

```bash
kubectl get nodes
# 等待新节点状态变为 Ready（约 1-2 分钟）
```

### ⚠️ hostPath 存储注意事项

当前 ClawManager 使用 **hostPath PV**（数据目录 `/tmp/clawreef/...`），PVC 数据存在**首次创建 Pod 的那台节点本地**。ClawManager 在创建 PV 时会写入 `nodeAffinity = <node-hostname>`，所以：

- ✅ 同一个 OpenClaw 实例的 Pod 即使重启 / 重建，K8s 会把它**继续调度回原节点**，数据连续
- ⚠️ 一旦那台节点宕机或被 `kubectl delete node`，**Pod 永远 Pending**（PV 找不到节点）；数据也只在那台机器的本地盘上
- ⚠️ 历史实例数据**不会自动迁移到新加入的 worker**

**新加入的 Worker 节点能干什么：**

| 工作负载 | 调度 |
|---|---|
| 新创建的 OpenClaw 实例 | ✅ ClawManager 调度器会按可用资源分配，可能落到新 Worker |
| 已有的 OpenClaw 实例 | ❌ 仍绑死在原节点，扩容 Worker 不影响 |
| 无状态 Pod（系统组件） | ✅ K8s 默认调度 |

**结论**：只需 `join-node.sh` 加进集群即可，**不需要任何额外的 nodeSelector / label 配置**。新实例自然均衡。

**生产高可用建议**（可选）：
- 单节点机房 → 加 1 张机器走 `join-node.sh --from-master` 即可分摊新实例负载
- 数据高可用 → 替换 hostPath 为 Longhorn / NFS / Ceph（需改 ClawManager pvc_service.go 的 PV 模板）

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

通常是新 Worker 资源不足 / 镜像没拉到。排查：
```bash
kubectl get pod <pod-name> -o wide
kubectl describe pod <pod-name>   # 查 Events，关注 FailedScheduling / ImagePullBackOff
kubectl get nodes -o wide
```

如果是历史实例（创建时绑了已被删除的节点），需要在 yunwu 后台删除该实例后重新创建。

### Q: --from-master 报 "Permission denied (publickey)"

新 Worker 上没配置 SSH 免密。执行：
```bash
ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519
ssh-copy-id root@<master-ip>   # 输入一次 master 密码即可
```
之后再跑 `--from-master`。

### Q: kubeadm join 报 "[ERROR FileAvailable--etc-kubernetes-pki-ca.crt]"

新 Worker 之前 join 过其他集群，残留文件。先 reset：
```bash
kubeadm reset -f
rm -rf /etc/cni/net.d /etc/kubernetes /var/lib/kubelet
```
再跑 `join-node.sh`。

### Q: 拆 Worker 给另一个项目独立用怎么办？

例如把当前集群的某台 Worker 抽出来给新项目（独立 yunwu 实例 + 独立 ClawManager 机房）：
```bash
# 1) 在原集群 Master 上移除该节点
kubectl delete node <hostname>

# 2) 在被抽出的机器上彻底清场
kubeadm reset -f
rm -rf /etc/cni/net.d /etc/kubernetes /var/lib/etcd /var/lib/kubelet \
       /tmp/clawreef /tmp/clawmanager /root/.kube
iptables -F && iptables -t nat -F

# 3) 跑 install-region.sh 当新机房的 Master 用
sudo ./install-region.sh <NEW_REGION_CODE>
```
⚠️ 抽走前确保该节点上没有用户数据 PVC（hostPath 数据会留在原盘但失效）。
