# Installs the Casebox CLI on Windows from its GitHub release:
#   irm https://casebox-docs.pages.dev/install.ps1 | iex
# $env:CASEBOX_VERSION picks a release (default: the newest, pre-releases included).
# $env:CASEBOX_INSTALL_DIR picks the directory (default: %LOCALAPPDATA%\casebox\bin).
$ErrorActionPreference = 'Stop'
$repo = 'alternayte/casebox'
$dir = if ($env:CASEBOX_INSTALL_DIR) { $env:CASEBOX_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'casebox\bin' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }

$tag = $env:CASEBOX_VERSION
if (-not $tag) {
  # The releases list includes pre-releases, which /releases/latest leaves out.
  $tag = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases?per_page=1")[0].tag_name
}
if (-not $tag.StartsWith('v')) { $tag = "v$tag" }
$version = $tag.Substring(1)
$name = "casebox_${version}_windows_$arch"
$base = "https://github.com/$repo/releases/download/$tag"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "Downloading Casebox $version for windows/$arch"
  Invoke-WebRequest "$base/$name.zip" -OutFile "$tmp\$name.zip" -UseBasicParsing
  Invoke-WebRequest "$base/checksums.txt" -OutFile "$tmp\checksums.txt" -UseBasicParsing
  $want = (Select-String -Path "$tmp\checksums.txt" -Pattern " $name.zip$").Line.Split(' ')[0]
  $got = (Get-FileHash "$tmp\$name.zip" -Algorithm SHA256).Hash.ToLower()
  if ($want -ne $got) { throw "checksum mismatch for $name.zip" }
  Expand-Archive "$tmp\$name.zip" -DestinationPath $tmp
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  Copy-Item "$tmp\$name\casebox.exe" (Join-Path $dir 'casebox.exe') -Force
} finally {
  Remove-Item -Recurse -Force $tmp
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($userPath -split ';') -notcontains $dir) {
  [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
  Write-Host "Added $dir to your PATH; open a new terminal to use casebox."
}
Write-Host "Installed $(Join-Path $dir 'casebox.exe') ($version)"
