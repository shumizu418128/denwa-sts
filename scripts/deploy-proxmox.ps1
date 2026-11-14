# このリポジトリの変更を Proxmox (SSH ホスト名 proxmox) に反映する。
# 実行: リポジトリ直下で .\scripts\deploy-proxmox.ps1
$ErrorActionPreference = "Stop"

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
$RemoteHost = "proxmox"
$RemoteBuild = "/tmp/denwa-sts-build"

Push-Location $RepoRoot
try {
	ssh -o BatchMode=yes $RemoteHost "rm -rf $RemoteBuild && mkdir -p $RemoteBuild"

	# .git やローカルビルド成果物を除いてソースを送る
	tar -cf - `
		--exclude=.git `
		--exclude=denwa-sts `
		--exclude=sts `
		--exclude=*.exe `
		. | ssh $RemoteHost "cd $RemoteBuild && tar -xf -"

	# ビルドスクリプトは送ったツリーから実行
	ssh $RemoteHost "chmod +x $RemoteBuild/scripts/proxmox-build.sh && DENWA_STS_BUILD_DIR=$RemoteBuild bash $RemoteBuild/scripts/proxmox-build.sh"
}
finally {
	Pop-Location
}