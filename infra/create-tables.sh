#!/usr/bin/env bash
# Create the DynamoDB tables required by EduCaption.
#
# Prereqs: aws CLI v2 authenticated against the target account.
# Usage:   AWS_REGION=ap-southeast-2 ./infra/create-tables.sh
set -euo pipefail

REGION="${AWS_REGION:-ap-southeast-2}"

cache_table() {
  local name="StudyMind_Cache"
  if aws dynamodb describe-table --table-name "$name" --region "$REGION" >/dev/null 2>&1; then
    echo "[skip] $name already exists"
    return
  fi
  echo "[create] $name"
  aws dynamodb create-table \
    --region "$REGION" \
    --table-name "$name" \
    --attribute-definitions \
      AttributeName=PK,AttributeType=S \
      AttributeName=SK,AttributeType=S \
    --key-schema \
      AttributeName=PK,KeyType=HASH \
      AttributeName=SK,KeyType=RANGE \
    --billing-mode PAY_PER_REQUEST >/dev/null
  aws dynamodb wait table-exists --table-name "$name" --region "$REGION"
  echo "[ready] $name"
}

users_table() {
  local name="StudyMind_Users"
  if aws dynamodb describe-table --table-name "$name" --region "$REGION" >/dev/null 2>&1; then
    echo "[skip] $name already exists"
    return
  fi
  echo "[create] $name (with email, google_sub, wechat_openid GSIs)"
  aws dynamodb create-table \
    --region "$REGION" \
    --table-name "$name" \
    --attribute-definitions \
      AttributeName=PK,AttributeType=S \
      AttributeName=SK,AttributeType=S \
      AttributeName=email,AttributeType=S \
      AttributeName=google_sub,AttributeType=S \
      AttributeName=wechat_openid,AttributeType=S \
    --key-schema \
      AttributeName=PK,KeyType=HASH \
      AttributeName=SK,KeyType=RANGE \
    --global-secondary-indexes \
      '[
        {"IndexName":"email-index","KeySchema":[{"AttributeName":"email","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}},
        {"IndexName":"google-sub-index","KeySchema":[{"AttributeName":"google_sub","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}},
        {"IndexName":"wechat-openid-index","KeySchema":[{"AttributeName":"wechat_openid","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}
      ]' \
    --billing-mode PAY_PER_REQUEST >/dev/null
  aws dynamodb wait table-exists --table-name "$name" --region "$REGION"
  echo "[ready] $name"
}

cache_table
users_table
echo "Done."
