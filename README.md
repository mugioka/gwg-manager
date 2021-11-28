![golangci-lint](https://github.com/mugioka/gcp-group-manager/actions/workflows/golangci-lint.yaml/badge.svg?branch=master)
## What is GWGManager
GWGManager is SlackBot for managing Google Workspace Groups with ChatOps.
Now that is intended to be deployed in GCP CloudRun.

## Demo
![demo (1)](https://user-images.githubusercontent.com/62197019/134805434-519f0925-dcdc-4b03-be8e-1f233a0990b7.gif)

## Usage
### Create a ServiceAccount for GWGManager
```
gcloud deployment-manager deployments create serviceaccount-iam-group-manager --config deployment-manager/serviceaccount.yaml --project YOUR_PROJECT
```
### Store the four environment variables for GWGManager to the secret manager.

|Name|Description|
|---|---|
|SLACK_APP_TOKEN|App level token for slack bot|
|SLACK_BOT_TOKEN|OAuth Token for slack bot|
|ORG_CUSTOMER_ID|GCP Organization ID|
|APPROVER_GROUP_ID|Slack user group ID who allows to join the group|

#### How to get your organization id
```
gcloud organizations list --format json | jq '.[].owner.directoryCustomerId
```
#### How to get the slack user group id
visit [here](https://api.slack.com/methods/usergroups.list)

### Deploy GWGManager to CloudRun
```
gcloud beta run services replace cloudrun/service.yaml --region YOUR_REGION --project YOUR_PROJECT
```
## How to build and push
```
gcloud auth configure-docker
docker build -t gcr.io/YOUR_PROJECT/iam-group-manager:latest .
docker push gcr.io/YOUR_PROJECT/iam-group-manager:latest
```
## How to develop
- Install an asdf if you did not install it.
  - [docs](http://asdf-vm.com/guide/getting-started.html#_1-install-dependencies)
- Install tools
  ```bash
  $ awk '{ print $1; }' .tool-versions | while read tool; do asdf plugin add ${tool}; done; asdf install
  ```
- Start server
  ```bash
  export SLACK_APP_TOKEN=YOUR_APP_TOKEN
  export SLACK_BOT_TOKEN=YOUR_BOT_TOKEN
  export ORG_CUSTOMER_ID=YOUR_ORG_CUSTOMER_ID
  export APPROVER_GROUP_ID=YOUR_APPROVER_GROUP_ID
  go mod download
  go run main.go
  ```

