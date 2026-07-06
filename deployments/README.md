# ClawManager 部署速查

> 一页讲清楚：**新机房怎么装** / **怎么加 Worker** / **怎么把 Worker 拆给新项目**。
> 详细文档 → [`scripts/region-setup.md`](scripts/region-setup.md)

---

## 目录结构

```
deployments/
├── README.md                  ← 本文件（速查）
├── k8s/clawmanager.yaml       ← K8s 部署清单（被 install-region.sh 拉取并渲染）
├── k3s/clawmanager.yaml       ← K3s 部署清单（保留，已不主推）
├── nginx/                     ← Nginx 反向代理示例
└── scripts/
    ├── install-region.sh      ← 新机房一键安装（kubeadm + ClawManager）
    ├── join-node.sh           ← 同集群加 Worker 节点
    └── region-setup.md        ← 完整部署手册
```

---

## 场景 1：装一个新机房（含新项目独立部署）

> 一台全新 Debian 12 / Ubuntu 22.04，root 权限。

```bash
# 在新服务器上
curl -sSL https://raw.githubusercontent.com/handsomeboy0612/clawmanager-custom/main/deployments/scripts/install-region.sh \
  -o install-region.sh
chmod +x install-region.sh

sudo ./install-region.sh <REGION_CODE>
# 例如：sudo ./install-region.sh xl-hk-1
```

**约 5–15 分钟**完成。脚本会输出摘要并写入 `/root/clawmanager-region-<CODE>.txt`：

```text
【yunwu 机房管理后台填写内容（三项必填）】
  机房代码:                  xl-hk-1
  ClawManager Base URL:       https://<server-ip>:30443
  ClawManager External API Key: claw-<48 位随机串>
```

把这三项填到 yunwu 后台「OpenClaw 机房管理 → 新建机房」→「测试连接」→ 保存，**完成**。

### REGION_CODE 命名规则

- 以小写字母开头
- 仅含小写字母 / 数字 / 连字符（`-`）
- 长度 2–32

✅ `cn-bj-1` / `us-lax-1` / `xl-hk-1`
❌ `1cn` / `CN_BJ` / `cn.bj.1`

---

## 场景 2：给现有机房加 Worker 节点（横向扩容）

### 前置：Worker → Master SSH 免密（一次性配置）

```bash
# 在新 Worker 上执行
ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519   # 已有可跳过
ssh-copy-id root@<master-ip>                            # 输入一次 master 密码
ssh root@<master-ip> 'echo ok'                          # 验证免密成功
```

### 一行加节点

```bash
# 在新 Worker 上执行
curl -sSL https://raw.githubusercontent.com/handsomeboy0612/clawmanager-custom/main/deployments/scripts/join-node.sh \
  -o join-node.sh
chmod +x join-node.sh

sudo bash join-node.sh --from-master root@<master-ip>
```

脚本会：
1. 通过 SSH 自动从 Master 拉取 `kubeadm join` 命令（token 24h 有效）
2. 安装 containerd + kubeadm + kubelet
3. 执行 join，等 kubelet 稳定
4. **失败时自动 dump kubelet/containerd 日志**，给出排查方向

**3–5 分钟**完成。在 Master 上 `kubectl get nodes` 确认新节点 Ready。

### 没有 SSH 免密时的备用方式

```bash
# Master 上拿 join 命令
ssh root@<master-ip> 'kubeadm token create --print-join-command'

# Worker 上：
sudo JOIN_CMD="<贴上面输出>" bash join-node.sh

# 或交互方式
sudo bash join-node.sh
```

---

## 场景 3：把 Worker 拆出来给另一个项目当独立机房

> 例：当前 yunwu 用 3 台（master + 2 worker），想把其中 1 台 worker 拆出来，给独立部署的 xiangliang 当全新机房。

### 前提确认

⚠️ **该 Worker 上不能有用户数据**。OpenClaw 实例的 PVC 是 hostPath，绑定了节点 hostname，拆走后数据找不到节点会失效。

```bash
# 在 yunwu master 上确认该 worker 上跑了哪些 pod
kubectl get pods -A -o wide | grep <worker-hostname>

# 如果有 OpenClaw 实例，先在 yunwu 后台联系用户删除
```

### 拆分步骤

```bash
# Step 1: 在原集群 master 上移除节点
ssh root@<old-master-ip>
kubectl delete node <worker-hostname>

# Step 2: 在被拆走的机器上彻底清场
ssh root@<worker-ip>
kubeadm reset -f
rm -rf /etc/cni/net.d /etc/kubernetes /var/lib/etcd /var/lib/kubelet \
       /root/.kube /tmp/clawreef /tmp/clawmanager
iptables -F && iptables -t nat -F && iptables -X
systemctl stop containerd && rm -rf /var/lib/containerd && systemctl start containerd

# Step 3: 当新机房 master 装
sudo ./install-region.sh <NEW_REGION_CODE>
# 输出新的三项 → 填到 xiangliang 后台
```

---

## 多项目部署架构图

```
yunwu 项目                          xiangliang 项目
─────────────                      ─────────────
yunwu 后端 (api.yunwu.ai)          xiangliang 后端 (api.xiangliang.com)
  │ MySQL: yunwu DB                  │ MySQL: xiangliang DB
  │ SELF_BASE_URL=api.yunwu.ai       │ SELF_BASE_URL=api.xiangliang.com
  │                                   │
  └─→ 机房 cn-bj-1                   └─→ 机房 xl-hk-1
       104.194.10.22 (master)            104.194.9.38 (master)
       104.243.32.50 (worker)            （未来按需加 worker）
       └─ 独立 ClawManager + 独立 K8s    └─ 独立 ClawManager + 独立 K8s
          + 独立 API Key                    + 独立 API Key
```

**关键事实**：

- **每个机房 = 一个独立 K8s 集群 + 独立 ClawManager + 独立 API Key**
- **每个项目（yunwu / xiangliang）= 独立后端 + 独立 MySQL + 独立 SELF_BASE_URL**
- 机房和项目的关系通过 yunwu DB 里的 `openclaw_regions` 表绑定（URL + API Key）
- `install-region.sh` 每次跑都生成全新随机的 API Key，**机房之间天然零交叉**

---

## 关于 `SELF_BASE_URL` / `CLAWMANAGER_*` 环境变量

| 变量 | 在哪配 | 作用 |
|---|---|---|
| `SELF_BASE_URL` | yunwu / xiangliang 项目的 `.env` | 容器内 OpenClaw 回调 yunwu 的 LLM API 域名 |
| `CLAWMANAGER_EXTERNAL_API_KEY` | ClawManager Pod 的 env（`install-region.sh` 自动注入） | yunwu 调用 ClawManager API 的鉴权 Key |
| `CLAWMANAGER_BASE_URL` | **已废弃**（多机房后改为 DB 表） | — |

→ yunwu 项目 `.env` 只需配 `SELF_BASE_URL`，**不再配** ClawManager 相关变量。
→ 机房的 URL 和 API Key 在 yunwu 后台「OpenClaw 机房管理」里维护。

---

## 故障排查速查

| 症状 | 排查 |
|---|---|
| install-region.sh 卡在 "Node Ready" | `kubectl -n kube-flannel get pods` 看 Flannel 是否拉到镜像 |
| install-region.sh 中途失败 | 脚本会自动 `kubeadm reset` 回滚（5s 内可 Ctrl+C 取消） |
| 测试连接失败"无法访问" | `curl -sk https://<ip>:30443/api/v1/instances` 在 yunwu 服务器上验证 |
| 测试连接失败"API Key 无效" | `kubectl get deployment clawmanager-app -n clawmanager-system -o yaml \| grep EXTERNAL_API_KEY` |
| join-node 失败 | 脚本会自动 dump kubelet/containerd 日志，按提示排查 |

更多见 [`scripts/region-setup.md`](scripts/region-setup.md) 末尾「常见问题」。
