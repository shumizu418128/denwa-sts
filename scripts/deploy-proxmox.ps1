# このリポジトリの変更を Proxmox (SSH ホスト名 proxmox) に反映する。
# 実行: リポジトリ直下で .\scripts\deploy-proxmox.ps1
$ErrorActionPreference = "Stop"

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
$RemoteHost = "proxmox"
$RemoteBuild = "/tmp/denwa-sts-build"
$Archive = Join-Path $env:TEMP "denwa-sts-src.tar"

function Assert-LastExit {
	param([string]$What)
	if ($LASTEXITCODE -ne 0) {
		throw "${What}に失敗しました (exit $LASTEXITCODE)"
	}
}

# Windows の tar はディレクトリを 0555 で記録する。そのまま展開すると中にファイルを作れない。
function Repair-TarDirectoryModes {
	param([string]$Path)
	$bytes = [System.IO.File]::ReadAllBytes($Path)
	$block = 512
	$offset = 0
	while ($offset + $block -le $bytes.Length) {
		$empty = $true
		for ($i = 0; $i -lt $block; $i++) {
			if ($bytes[$offset + $i] -ne 0) {
				$empty = $false
				break
			}
		}
		if ($empty) {
			break
		}
		if ($bytes[$offset + 156] -eq [byte][char]'5') {
			$mode = [System.Text.Encoding]::ASCII.GetBytes("000755 ")
			[Array]::Copy($mode, 0, $bytes, $offset + 100, 7)
			$bytes[$offset + 107] = 0
			for ($i = 148; $i -lt 156; $i++) {
				$bytes[$offset + $i] = 32
			}
			$sum = 0
			for ($i = 0; $i -lt $block; $i++) {
				$sum += $bytes[$offset + $i]
			}
			$octal = [Convert]::ToString($sum, 8).PadLeft(6, '0')
			$checksum = [System.Text.Encoding]::ASCII.GetBytes($octal)
			[Array]::Copy($checksum, 0, $bytes, $offset + 148, 6)
			$bytes[$offset + 154] = 0
			$bytes[$offset + 155] = 32
		}
		$sizeText = [System.Text.Encoding]::ASCII.GetString($bytes, $offset + 124, 12).Trim([char]0, ' ')
		$size = 0
		if ($sizeText) {
			$size = [Convert]::ToInt64($sizeText, 8)
		}
		$dataBlocks = [int][Math]::Ceiling($size / $block)
		$offset += $block * (1 + $dataBlocks)
	}
	[System.IO.File]::WriteAllBytes($Path, $bytes)
}

Push-Location $RepoRoot
try {
	if (Test-Path $Archive) {
		Remove-Item $Archive -Force
	}

	# 前回の展開でディレクトリが 0555 だと、書き込み権が無く rm できない
	ssh -o BatchMode=yes $RemoteHost "if [ -d $RemoteBuild ]; then chmod -R u+w $RemoteBuild; fi; rm -rf $RemoteBuild && mkdir -p $RemoteBuild"
	Assert-LastExit "リモート作業ディレクトリの作成"

	# .git と Windows のビルド成果物を除く。pax 拡張ヘッダを避けるため ustar で作る。
	# denwa-sts という名前では除外しない。cmd/denwa-sts まで落ちる。
	tar --format=ustar -cf $Archive `
		--exclude=.git `
		--exclude=*.exe `
		.
	Assert-LastExit "アーカイブ作成"
	Repair-TarDirectoryModes $Archive

	scp -o BatchMode=yes $Archive "${RemoteHost}:${RemoteBuild}/src.tar"
	Assert-LastExit "アーカイブ送信"

	$remote = "set -euo pipefail; cd $RemoteBuild; tar -xf src.tar; rm -f src.tar; test -f internal/lfm/segment.go; chmod +x scripts/proxmox-build.sh; DENWA_STS_BUILD_DIR=$RemoteBuild bash scripts/proxmox-build.sh"
	# gcc はログイン時の PATH（~/.local/bin）にだけ入っている
	ssh -o BatchMode=yes $RemoteHost "bash -lc '$remote'"
	Assert-LastExit "リモートビルド"
}
finally {
	Pop-Location
	if (Test-Path $Archive) {
		Remove-Item $Archive -Force
	}
}
