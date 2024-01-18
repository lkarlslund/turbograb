function BuildVariants {
  param (
    $ldflags,
    $compileflags,
    $suffix,
    $folder
  )

  $env:GOARCH = "386"
  $env:GOOS = "windows"
  & $BUILDER build -ldflags "$ldflags" -o binaries/grabass-windows-386.exe $compileflags $folder

  $env:GOARCH = "amd64"
  $env:GOOS = "windows"
  & $BUILDER build -ldflags "$ldflags" -o binaries/grabass-windows-x64.exe $compileflags $folder
  $env:GOOS = "darwin"
  & $BUILDER build -ldflags "$ldflags" -o binaries/grabass-osx-x64 $compileflags $folder
  $env:GOOS = "linux"
  & $BUILDER build -ldflags "$ldflags" -o binaries/grabass-linux-x64 $compileflags $folder

  $env:GOARCH = "arm64"
  $env:GOOS = "windows"
  & $BUILDER build -ldflags "$ldflags" -o binaries/grabass-windows-arm64.exe $compileflags $folder
  $env:GOOS = "darwin"
  & $BUILDER build -ldflags "$ldflags" -o binaries/grabass-osx-arm64 $compileflags $folder
  $env:GOOS = "linux"
  & $BUILDER build -ldflags "$ldflags" -o binaries/grabass-linux-arm64 $compileflags $folder
}

Set-Location $PSScriptRoot

# Release
$BUILDER = "go"
BuildVariants -ldflags "$LDFLAGS -w -s" ./cmd/turbograb
# BuildVariants -ldflags "$LDFLAGS -w -s" ./cmd/turbogrep
