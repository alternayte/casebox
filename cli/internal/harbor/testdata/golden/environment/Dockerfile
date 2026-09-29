FROM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS casebox-verdict
WORKDIR /src
COPY verdict/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /casebox-verdict .

FROM golang:1.26-bookworm
USER root
RUN ["/bin/sh","-c","grep -q '^[^:]*:[^:]*:10001:' /etc/passwd || echo 'casebox:x:10001:10001:casebox:/home/casebox:/bin/sh' \u003e\u003e /etc/passwd; grep -q '^[^:]*:[^:]*:10001:' /etc/group || echo 'casebox:x:10001:' \u003e\u003e /etc/group; mkdir -p /home/casebox \u0026\u0026 chown 10001:10001 /home/casebox"]
RUN ["/bin/sh","-c","mkdir -p '/workspace' /results"]
WORKDIR "/workspace"
COPY ["files/","/workspace/"]
RUN ["/bin/sh","-c","go mod download"]
RUN ["/bin/sh","-c","chown -R 10001:10001 '/workspace' /results"]
USER 10001
USER root
COPY --from=casebox-verdict ["/casebox-verdict","/usr/local/bin/casebox-verdict"]
RUN ["/bin/sh","-c","rm -rf -- '.git' '.casebox' 'store/put_test.go' 'store/store_test.go'"]
COPY ["base/","/workspace/"]
COPY ["base.files","/tmp/casebox-base.files"]
RUN ["/bin/sh","-c","set -e\ncommand -v git \u003e/dev/null 2\u003e\u00261 || { echo 'the environment has no git: the recipe image or install steps must provide it, because the base tree is committed in the image' \u003e\u00262; exit 1; }\nexport GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'\nexport GIT_AUTHOR_NAME=casebox GIT_AUTHOR_EMAIL=casebox@localhost GIT_AUTHOR_DATE='946684800 +0000' GIT_COMMITTER_NAME=casebox GIT_COMMITTER_EMAIL=casebox@localhost GIT_COMMITTER_DATE='946684800 +0000'\ncd '/workspace'\ngit init -q\ngit config user.name casebox\ngit config user.email casebox@localhost\ngit config commit.gpgsign false\ngit -c core.hooksPath=/dev/null -c core.fsmonitor=false update-index --add -z --stdin \u003c/tmp/casebox-base.files\ncommit=$(git commit-tree -m base \"$(git write-tree)\")\ngit -c core.logAllRefUpdates=false symbolic-ref HEAD refs/heads/main\ngit -c core.logAllRefUpdates=false update-ref refs/heads/main \"$commit\"\nrm -rf .git/logs /tmp/casebox-base.files\nmkdir -p .git/info\ngit -c core.hooksPath=/dev/null -c core.fsmonitor=false -c core.quotePath=false ls-files --others --exclude-standard --directory | sed -e 's/[][*?\\\\]/\\\\\u0026/g' -e 's|^|/|' \u003e\u003e.git/info/exclude\ntest -z \"$(git -c core.hooksPath=/dev/null -c core.fsmonitor=false status --porcelain --untracked-files=all)\" || { echo 'the base repository is not clean after ignoring what the environment left' \u003e\u00262; exit 1; }\nchown -R 10001:10001 '/workspace' /results"]
USER 10001
