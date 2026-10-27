# Google OAuth 2.0 & OpenID Connect Integration

TeleMeetBot supports official **"Continue with Google"** authentication using OAuth 2.0 and OpenID Connect (OIDC). This replaces manual cookie handling with an automated, cryptographically secured identity workflow.

---

## 1. Google Cloud Console Setup

### Step A: Create or Select a Project
1. Navigate to the [Google Cloud Console](https://console.cloud.google.com/).
2. Create a new project (e.g. `TeleMeetBot`) or select an existing project.

### Step B: Configure the OAuth Consent Screen
1. Go to **APIs & Services** → **OAuth consent screen**.
2. Select **User Type**:
   - **Internal:** Recommended if you are deploying within a Google Workspace organization. Only users inside your organization can sign in.
   - **External:** Available for general Gmail accounts. In *Testing* publishing status, add your Google email address under **Test users**.
3. Fill in required application information (App name: `TeleMeetBot`, user support email, developer contact email).
4. Add the following **minimum required scopes**:
   - `openid`
   - `https://www.googleapis.com/auth/userinfo.email`
   - `https://www.googleapis.com/auth/userinfo.profile`

### Step C: Create OAuth 2.0 Client Credentials
1. Go to **APIs & Services** → **Credentials** → **Create Credentials** → **OAuth client ID**.
2. Select **Application type**: `Web application`.
3. Under **Authorized redirect URIs**, add your bot's public callback URL:
   - For public domain: `https://your-domain.com/auth/google/callback`
   - For local/testing: `http://127.0.0.1:18080/auth/google/callback`
4. Copy the generated **Client ID** and **Client Secret**.

---

## 2. TeleMeetBot Configuration

### Option 1: Environment Variables (Recommended)
Set the credentials in your environment or container configuration (e.g., in `.env` or systemd service unit):

```bash
TELEMEETBOT_GOOGLE_CLIENT_ID="your-client-id.apps.googleusercontent.com"
TELEMEETBOT_GOOGLE_CLIENT_SECRET="GOCSPX-your-client-secret"
TELEMEETBOT_GOOGLE_REDIRECT_URL="https://your-domain.com/auth/google/callback"
# Optional: 64-character hex string (32-byte key). If omitted, an AES-256 key is auto-generated in <data_dir>/secrets/oauth_cipher.key
TELEMEETBOT_GOOGLE_TOKEN_ENCRYPTION_KEY=""
```

### Option 2: `telemeetbot.yml` Configuration
```yaml
google:
  enabled: true
  client_id: "your-client-id.apps.googleusercontent.com"
  client_secret: "GOCSPX-your-client-secret"
  redirect_url: "https://your-domain.com/auth/google/callback"
  token_encryption_key: ""
  scopes:
    - "openid"
    - "https://www.googleapis.com/auth/userinfo.email"
    - "https://www.googleapis.com/auth/userinfo.profile"
```

---

## 3. Security Architecture

- **Authorization Code + PKCE (RFC 7636):** Uses a high-entropy cryptographic code verifier and SHA-256 code challenge (`S256`) to prevent authorization code interception and injection attacks.
- **HMAC-SHA256 Signed State:** The `state` parameter contains the Telegram User ID, random state token, and expiration timestamp signed with an internal HMAC secret. Validated with `hmac.Equal` and consumed immediately upon callback (single-use CSRF defense).
- **Nonce Replay Prevention:** A cryptographically random nonce is bound to the authorization request and verified against the `nonce` claim in the OpenID Connect ID token.
- **Server-Side Token & Identity Verification:** Validates `iss` (`https://accounts.google.com`), `aud` (`client_id`), and expiration (`exp`) before accepting any identity claims.
- **AES-256-GCM Token Encryption at Rest:** Access tokens, refresh tokens, and ID tokens are encrypted before being written to disk. The Additional Authenticated Data (AAD) binds each ciphertext to the specific Telegram User ID and Google Sub (`telemeetbot:user:<id>:sub:<sub>`), guaranteeing that tokens cannot be transplanted across users.
- **Zero Log Exposure:** Tokens and sensitive secrets are never logged or exposed in Telegram messages or URLs.
- **Automatic Token Rotation & Revocation:** Automatically refreshes expiring access tokens via the refresh token grant. Users can disconnect anytime with 1-click revocation (`https://oauth2.googleapis.com/revoke`).

---

## 4. Google Meet & Browser Authentication Analysis

### Technical Boundary
1. **Google OAuth 2.0 vs. Browser Session Cookies:**
   Google OAuth 2.0 is an API authorization framework (RFC 6749) that issues Bearer tokens for Google Cloud and Google Workspace APIs (such as the Google Meet REST API `https://meet.googleapis.com/v2`). Google **strictly prohibits** third-party OAuth 2.0 applications from exchanging OAuth Bearer tokens for first-party browser cookies (`SID`, `HSID`, `__Secure-1PSID`) on `accounts.google.com` or `meet.google.com`.
2. **Chromium Web Navigation in Google Meet:**
   Google Meet's web client (`https://meet.google.com`) authenticates users through first-party browser session cookies. There is no public or policy-compliant Google API that converts an OAuth 2.0 access token into active Chromium browser cookies.

### TeleMeetBot's Policy-Compliant Architecture
To comply with Google API and security policies without reverting to manual cookie dumping:
- **Verified Identity Presentation:** When a user signs in via "Continue with Google", TeleMeetBot binds their verified Google Identity (`Name` and `Email`). When launching a Google Meet session, TeleMeetBot enters the meeting using the verified identity (e.g., `Alex Morgan (alex@example.com)`). Meeting organizers see the authenticated name and email rather than an anonymous guest, ensuring trusted and immediate admission.
- **Google Meet REST API Integration:** The encrypted OAuth access token can be used with Google Workspace APIs to inspect space configurations, verify conference records, and check participant permissions when administrative scopes are granted.
