# A fake VPS for end-to-end tests: sshd + git + its own docker daemon (dind),
# so deploys behave exactly like on a real server, ports and all.
FROM docker:27-dind

RUN apk add --no-cache openssh-server openssh-sftp-server git curl bash \
    && ssh-keygen -A \
    && mkdir -p /root/.ssh /srv \
    && chmod 700 /root/.ssh \
    && sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
    && apk add --no-cache docker-cli-compose

COPY authorized_keys /root/.ssh/authorized_keys
RUN chmod 600 /root/.ssh/authorized_keys

COPY entry.sh /entry.sh
RUN chmod +x /entry.sh

EXPOSE 22
ENTRYPOINT ["/entry.sh"]
