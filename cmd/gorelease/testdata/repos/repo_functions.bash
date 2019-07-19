function repo_create {
  if [ $# -ne 1 ]; then
    echo "usage: repo_create name" >&2
    return 1
  fi
  local name=$1
  rm -rf "$name"
  mkdir "$name"
  cd "$name"
  git init
  go mod init "example.com/$name"
  git add go.mod
  git commit -m 'initial commit'
}

function repo_pack {
  if [ $# -ne 1 ]; then
    echo "usage: repo_pack name" >&2
    return 1
  fi
  local name=$1
  cd ..
  rm -f "$name.zip"
  zip --quiet -r "$name.zip" "$name"
  rm -rf "$name"
}
