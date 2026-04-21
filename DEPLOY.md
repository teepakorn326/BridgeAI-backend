# Deployment Guide

**Run commands from this folder (`backend/`)** unless noted. The frontend and transcript-service live in sibling folders (`../frontend/`, `../transcript-service/`) as independent repos (separate GitHub remotes).

> **Note on resource names.** A prior deploy attempt created resources under the old project name `educaption/*` / `educaption-*` (the JWT secret in Secrets Manager and two empty ECR repositories). The commands below use the new name `bridgeai/*` / `bridgeai-*`. Either reuse the existing `educaption-*` resources (substitute names in the commands) or delete them first. Reference: `educaption/jwt-secret`, `educaption-backend`, `educaption-transcript`.

Target: one production stack for the WSTI Hackathon demo.

- **Frontend** → Vercel
- **Backend (Go / Fiber)** → AWS App Runner
- **Transcript service (Python / FFmpeg / Whisper)** → AWS ECS Fargate behind an ALB
- **DynamoDB** → existing AWS account, tables created via `infra/create-tables.sh`
- **Secrets** → AWS Secrets Manager, wired into each service's runtime env

The primary account for the demo is `bosskero326@gmail.com` and is pre-seeded by `backend/cmd/seed`.

---

## 0. One-time prerequisites

- AWS CLI v2 authenticated against the target account (`aws sts get-caller-identity`)
- Docker running locally
- A region chosen — commands below assume `ap-southeast-2`. Bedrock cross-region inference profile `apac.anthropic.claude-sonnet-4-20250514-v1:0` is hardcoded in `backend/internal/services/bedrock.go`; pick a region in the apac profile.
- Bedrock model access approved for Claude Sonnet 4 in the chosen region.

```bash
export AWS_REGION=ap-southeast-2
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
```

---

## 1. Rotate the exposed secrets

The repo previously had real OAuth + JWT values in `backend/.env` on disk. Rotate before anything ships:

- Google Cloud Console → OAuth 2.0 Client → "Reset secret" for the OAuth client.
- Generate a new JWT secret: `openssl rand -base64 48`
- (WeChat if used) regenerate in the WeChat Open Platform.

Do **not** copy new values back into a committed file. They belong in Secrets Manager.

---

## 2. Create DynamoDB tables

```bash
./infra/create-tables.sh   # from backend/
```

Creates `StudyMind_Cache` and `StudyMind_Users` (with `email-index`, `google-sub-index`, `wechat-openid-index` GSIs), both pay-per-request. Idempotent — safe to re-run.

---

## 3. Store secrets in Secrets Manager

```bash
aws secretsmanager create-secret --name bridgeai/jwt-secret \
  --secret-string "$(openssl rand -base64 48)"

aws secretsmanager create-secret --name bridgeai/google-oauth \
  --secret-string '{"client_id":"…","client_secret":"…"}'

# Optional:
aws secretsmanager create-secret --name bridgeai/wechat-oauth \
  --secret-string '{"app_id":"…","app_secret":"…"}'
```

App Runner and ECS task definitions pull these in as environment variables at launch (see sections 5 and 6).

---

## 4. Build and push the two images

Create the ECR repos once:

```bash
aws ecr create-repository --repository-name bridgeai-backend    --region $AWS_REGION
aws ecr create-repository --repository-name bridgeai-transcript --region $AWS_REGION

aws ecr get-login-password --region $AWS_REGION | \
  docker login --username AWS --password-stdin ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com
```

Build (`linux/amd64` to match App Runner and Fargate — important when building on an Apple Silicon Mac):

```bash
# Backend — from backend/ folder
docker build --platform linux/amd64 \
  -t ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/bridgeai-backend:latest \
  .
docker push ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/bridgeai-backend:latest

# Transcript — from backend/ folder, pointing at sibling
docker build --platform linux/amd64 \
  -t ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/bridgeai-transcript:latest \
  ../transcript-service
docker push ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/bridgeai-transcript:latest
```

---

## 5. Deploy the transcript service (ECS Fargate)

App Runner can't run the transcript service reliably: it needs FFmpeg on disk, does long downloads with yt-dlp, and Whisper's first cold model load is slow. Fargate is the right fit.

Rough shape — use the AWS console or CDK/Terraform:

- **Cluster**: `bridgeai`
- **Task definition** (`bridgeai-transcript`):
  - CPU 1 vCPU, Memory 2 GB (2 vCPU / 4 GB if you use a larger Whisper model)
  - Container port 8081
  - Task role: `AmazonECSTaskExecutionRolePolicy`
  - No extra IAM needed (no AWS API calls from this service today)
- **Service**:
  - 1 task, public subnets (for yt-dlp egress), security group open on 8081 from the ALB SG
  - Behind an internal ALB with target group `bridgeai-transcript-tg`, health check `/health` (add a `/health` route to `main.py` if it isn't there yet)
- Note its DNS name — that becomes `TRANSCRIPT_SERVICE_URL` for the backend.

Cold-start note: the first request downloads the Whisper model (~140 MB for `base`). If you want to pre-warm, add an EFS volume mounted at `/app/.cache` so the model persists across task restarts.

---

## 6. Deploy the backend (App Runner)

Console → App Runner → Create service:

- **Source**: ECR → `bridgeai-backend:latest`
- **Port**: 8080
- **Instance role**: custom role with
  - `AmazonDynamoDBFullAccess` (scope tighter later: just the two tables)
  - `AmazonBedrockFullAccess` (scope tighter later: just `InvokeModel` on the Claude Sonnet model)
  - `secretsmanager:GetSecretValue` on the three secrets above
- **Environment**:

  | Key | Value |
  |---|---|
  | `APP_ENV` | `production` |
  | `AWS_REGION` | `ap-southeast-2` |
  | `PORT` | `8080` |
  | `CORS_ORIGINS` | `https://your-frontend.vercel.app` |
  | `FRONTEND_URL` | `https://your-frontend.vercel.app` |
  | `TRANSCRIPT_SERVICE_URL` | `http://<transcript-alb-dns>` |
  | `JWT_SECRET` | pulled from secret `bridgeai/jwt-secret` |
  | `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | from secret `bridgeai/google-oauth` |
  | `GOOGLE_REDIRECT_URL` | `https://<apprunner-domain>/auth/google/callback` |

- **Health check**: `/api/health`

App Runner hands you a `*.awsapprunner.com` URL. That's the backend base.

Update Google OAuth console to whitelist the new `GOOGLE_REDIRECT_URL`.

---

## 7. Seed the demo account

After the backend is live and tables exist, run the seeder once. Easiest path: exec into an App Runner instance is not supported, so run locally with your AWS creds pointed at the same account:

```bash
# from backend/
export AWS_REGION=ap-southeast-2
export SEED_EMAIL=bosskero326@gmail.com
export SEED_PASSWORD='<pick something strong, 10+ chars>'
export SEED_FIRST_NAME=Boss
go run ./cmd/seed
```

Or run the seed binary baked into the backend image as a one-off ECS task:

```bash
docker run --rm \
  -e AWS_REGION=$AWS_REGION \
  -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID \
  -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY \
  -e SEED_EMAIL=bosskero326@gmail.com \
  -e SEED_PASSWORD='<strong>' \
  --entrypoint /app/seed \
  ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/bridgeai-backend:latest
```

Re-running the seeder updates the password in place — safe to use for rotations.

---

## 8. Deploy the frontend (Vercel)

```bash
cd ../frontend
npx vercel --prod
```

Environment variable on Vercel: set `NEXT_PUBLIC_API_BASE` (if your `frontend/src/lib/api.ts` reads one — check there and wire it up if not) to the App Runner URL from step 6.

CORS check: the backend's `CORS_ORIGINS` must include the Vercel URL.

---

## 9. Ship the Chrome extension

One line to change before packaging: `chrome-extension/config.js` → set `DEFAULT_API_BASE` to the App Runner URL. Then:

- Zip the `chrome-extension/` folder
- Chrome Web Store → developer dashboard → upload

Users who already installed will need to either reinstall or manually update the API URL in the popup settings (storage.sync retains the old value across updates).

Also update `chrome-extension/manifest.json` `host_permissions` — replace `http://localhost:8080/*` with your App Runner domain.

---

## Post-deploy checklist

- [ ] `curl https://<apprunner>/api/health` returns 200
- [ ] Transcript `/health` returns 200 (add the route if missing)
- [ ] Log in as `bosskero326@gmail.com` from the Vercel URL
- [ ] Paste a YouTube URL on `/home` → bilingual subtitles render within ~30s
- [ ] Chrome extension on an Echo360 / Coursera / Udemy page translates correctly
- [ ] `/home` shows courses across four platforms
- [ ] EN ⇄ 中文 toggle in the Navbar works on landing and home

## Known follow-ups

These are deferred — flagged in the code review, not blocking the demo:

- `ListCachedCourses` in `backend/internal/database/dynamo.go` does a full table scan on every `/api/courses` call. Acceptable at demo scale; migrate to a GSI or a separate courses table before scale.
- Bedrock model + region are hardcoded in `backend/internal/services/bedrock.go`. Parameterise via env before multi-region.
- Extension popup/sidepanel HTML still hardcode `localhost:8080` as the `<input>` default value; `config.js` only wins for the service-worker path. Not harmful (user can override in UI), but clean this up if time permits.
