# Security

Security fixes target the latest release; during private preview, they target
`main`. Claude's transport is experimental and depends on an undocumented native
interface. Local capabilities prevent accidental session identity mixing; they
do not defend against hostile processes running as the same OS user.

Use [GitHub private vulnerability reporting](https://github.com/zalkilabs/zalki/security/advisories/new)
when the repository's **Report a vulnerability** option is available. If it is
unavailable, open an issue requesting a private reporting channel, without
technical details of the vulnerability.

Include affected versions, impact, and a minimal reproduction in the private
report. Do not publish credentials, session capabilities, private message bodies,
or personal paths. Coordinate disclosure with the maintainer before making
exploitation details public.
