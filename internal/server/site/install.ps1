# exe installer for Windows (x86-64):
#
#   irm https://exe.v2core.com/install.ps1 | iex
#
# It downloads the latest release from GitHub, checks it against the
# release's SHA256SUMS and hands over to `exe setup`, which asks where exe
# should listen, whether it needs a token, whether to install the extra
# apps, whether to set the PC up for VMs (the hypervisor platform and
# QEMU: the one part that needs an administrator) and which drive the VMs
# should be kept on, and only then writes anything. exe goes to
# %LOCALAPPDATA%\Programs\exe, its data to %USERPROFILE%\.exe, and it
# starts when you sign in.
#
# With nobody at a keyboard it asks nothing and installs for this machine
# only; the answers can be given ahead of time, as environment variables:
#
#   EXE_INSTALL_LISTEN = local | all | tailscale    EXE_INSTALL_TOKEN = yes | no
#   EXE_INSTALL_APPS = yes | no                      EXE_INSTALL_VMS = yes | no
#   EXE_INSTALL_VM_DRIVE = a drive letter            EXE_API_TOKEN = the token to use
#
# Docs: https://exe.v2core.com/docs/getting-started
#
# Everything is inside one block, run on the last line: a download cut
# short runs nothing. It never calls exit, which would close the terminal
# it was pasted into.
& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $base = 'https://github.com/livid/exe/releases'
    if ($env:EXE_RELEASE_URL) { $base = $env:EXE_RELEASE_URL.TrimEnd('/') }

    # a 32-bit PowerShell on a 64-bit Windows says x86 of itself
    $arch = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }
    if ($arch -ne 'AMD64') { throw "exe install: there is no exe release for Windows on ${arch}: x86-64 only" }
    if (-not (Get-Command tar.exe -ErrorAction SilentlyContinue)) {
        throw 'exe install: tar.exe is needed and was not found (Windows 10 version 1803 and later have it)'
    }

    # The latest release is where /latest redirects to: .../tag/<version>.
    $request = [Net.HttpWebRequest]::Create("$base/latest")
    $request.AllowAutoRedirect = $false
    try { $response = $request.GetResponse() } catch { throw "exe install: could not reach $base" }
    $location = [string]$response.Headers['Location']
    $response.Close()
    if ($location -notmatch '/tag/(\d+\.\d+\.\d+(\.\d+)?)$') { throw 'exe install: no release of exe has been published yet' }
    $version = $Matches[1]

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('exe-install-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $binary = 'exe-windows-amd64.tar.gz'
        Write-Host "Downloading exe $version..."
        foreach ($file in 'SHA256SUMS', $binary, 'exe-apps.tar.gz') {
            try {
                Invoke-WebRequest -UseBasicParsing "$base/download/$version/$file" -OutFile (Join-Path $tmp $file)
            } catch {
                if ($file -eq $binary) { throw "exe install: exe $version has no build for Windows" }
                throw "exe install: could not download $file of exe $version"
            }
        }
        $sums = @{}
        foreach ($line in Get-Content (Join-Path $tmp 'SHA256SUMS')) {
            $parts = $line -split '\s+', 2
            if ($parts.Count -eq 2) { $sums[$parts[1].TrimStart('*')] = $parts[0] }
        }
        foreach ($file in $binary, 'exe-apps.tar.gz') {
            if (-not $sums[$file]) { throw "exe install: $file is not listed in the release's SHA256SUMS" }
            $sum = (Get-FileHash (Join-Path $tmp $file) -Algorithm SHA256).Hash
            if ($sum -ne $sums[$file]) { throw "exe install: $file does not match the release's checksum; nothing was installed" }
        }
        tar.exe -xzf (Join-Path $tmp $binary) -C $tmp
        if ($LASTEXITCODE -ne 0) { throw "exe install: could not unpack $binary" }

        # exe setup asks its questions in this terminal and says what it did
        & (Join-Path $tmp 'exe.exe') setup -from $tmp
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}
