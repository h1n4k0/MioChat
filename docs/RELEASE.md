# 发布与校验

## 发布

版本号由程序源码声明，Git 标签使用 `v` 前缀，两者必须一致。当前版本为 `v2.1.0`。

发布提交和标签使用 SSH 签名。维护者的签名公钥应包含在 `signing/allowed_signers`；私钥保留在本地或签名设备中。

```sh
git config gpg.format ssh
git config gpg.ssh.allowedSignersFile signing/allowed_signers
git config commit.gpgsign true
git config tag.gpgsign true
git config user.signingkey /path/to/signing-key
go test ./...
go vet ./...
git add .
git commit -S -m "Release v2.1.0"
git tag -s v2.1.0 -m "MioChat v2.1.0"
git verify-commit HEAD
git verify-tag v2.1.0
```

发布控制使用受保护的 `main` 分支。仓库应限制该分支与发布标签的写入权限，为 `release` environment 配置独立审核人、禁止自审，并仅允许该分支部署；签名公钥和工作流变更需要强制审查。

保护规则应要求 CODEOWNERS 审查（包括其自身变更），限制可绕过规则的身份；仅显示“受保护”或只禁止强推不足以替代这些授权边界。

确认签名、版本和工作区后推送提交及标签，并从 `main` 手动启动发布：

```sh
git push origin main
git push origin v2.1.0
gh workflow run release.yml --ref main -f tag=v2.1.0
```

Release 工作流从受保护的 `main` 读取验签代码和公钥，要求标签指向该次控制分支提交；不执行待验证标签自带的验签器。缺少分支保护时拒绝发布。通过检查后构建 Linux amd64、Linux arm64、Windows amd64 与源码包，生成 `SHA256SUMS` 和构建来源证明，并上传到草稿 Release。证明包命名为 `MioChat-v2.1.0-provenance.sigstore.json`。核对草稿中的资产后发布。

重跑会复用同一草稿，验证已有资产后只补传缺失文件；内容冲突、额外资产或已发布的 Release 会使流程停止，不覆盖或删除现有文件。

本地打包使用：

```sh
python scripts/build_release.py --version 2.1.0 --output dist
```

## 下载校验

下载对应压缩包与 `SHA256SUMS`，检查文件摘要：

```sh
sha256sum -c SHA256SUMS --ignore-missing
```

验证 GitHub 构建来源证明：

```sh
git verify-tag v2.1.0
release_commit="$(git rev-parse 'v2.1.0^{commit}')"
gh attestation verify MioChat-v2.1.0-linux-amd64.tar.gz \
  --repo mio-qwq/MioChat \
  --signer-workflow mio-qwq/MioChat/.github/workflows/release.yml \
  --source-ref refs/heads/main \
  --source-digest "$release_commit" \
  --deny-self-hosted-runners
```

也可加 `--bundle MioChat-v2.1.0-provenance.sigstore.json` 使用下载的证明包。

来源证明将发布文件与仓库、工作流及提交关联。Git 提交和标签签名可在完整克隆中用 `git verify-commit`、`git verify-tag` 校验；验证时使用仓库中固定的可信签名列表。
