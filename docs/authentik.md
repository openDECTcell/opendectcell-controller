# Signing in with Authentik (or any OpenID Connect provider)

The controller speaks OpenID Connect (authorization code with PKCE) and
creates a user at the first login. Authentik is the tested example; Keycloak,
Zitadel and others work the same way. Settings are environment variables or
the YAML file (`docs/configuration.md`), not a UI page: a wrong value there
could lock every admin out.

## In Authentik

1. **Applications → Providers → Create → OAuth2/OpenID Provider.**
   - Client type: **Confidential**.
   - Redirect URI (strict): `https://dect.example.org/auth/oidc/callback`, the
     controller's `OPENDECTCELL_HTTP_BASE_URL` plus `/auth/oidc/callback`.
   - Scopes: `openid`, `profile`, `email`. Authentik puts the `groups` claim in
     the ID token without a further scope.
   - Subject mode: the default (hashed user id) is fine; it must stay the same.
2. **Create an Application** for the provider, with a slug, e.g. `opendectcell`.
3. Note the **client ID**, the **client secret** and the **issuer**:
   `https://auth.example.org/application/o/opendectcell/` (with the trailing slash).
4. Make groups: for example `dect-admins` and `dect-users`, and bind them to
   the application so only they can reach it.

## In the controller

```
OPENDECTCELL_OIDC_ISSUER=https://auth.example.org/application/o/opendectcell/
OPENDECTCELL_OIDC_CLIENT_ID=...
OPENDECTCELL_OIDC_CLIENT_SECRET_FILE=/run/secrets/oidc_client_secret
OPENDECTCELL_OIDC_ADMIN_GROUPS=dect-admins
OPENDECTCELL_OIDC_USER_GROUPS=dect-users
OPENDECTCELL_OIDC_PROVIDER_NAME=Sign in with Authentik
```

- With `USER_GROUPS` empty **every user Authentik authenticates for this
  application may log in**. Set it, or restrict access in Authentik.
- The role is recomputed from the groups at every login: removing someone from
  `dect-admins` takes admin rights away at their next login.
- Local accounts can stay on as a break-glass admin
  (`OPENDECTCELL_AUTH_LOCAL_ENABLED=true`, the default) or be turned off.
- Self-registration (Settings page) is for local accounts only; with Authentik
  the provider's own enrolment flows do that job.
- Single sign-on users have no password here; their password page says so.

## Checking

Settings → Sign-in shows whether single sign-on is on and the issuer in use.
A failed login is logged in the controller's log (component `auth`) and the
login page says why in general terms only.
