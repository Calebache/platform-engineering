variable "namespace" {
  type    = string
  default = "argocd"
}

variable "chart_version" {
  description = "argo-cd Helm chart version (argoproj/argo-helm)."
  type        = string
}

variable "apps_chart_version" {
  description = "argocd-apps Helm chart version, used to create the root Application."
  type        = string
}

variable "repo_name" {
  description = "Repository (owner/name); used to name the credential Secret."
  type        = string
}

variable "repo_url" {
  description = "HTTPS URL of the GitOps repository (GitHub App auth requires HTTPS), e.g. https://github.com/owner/name.git."
  type        = string

  validation {
    condition     = startswith(var.repo_url, "https://")
    error_message = "GitHub App credentials only work with an https:// repo URL."
  }
}

variable "github_app" {
  description = "Read-only GitHub App credentials for repo_url. Ephemeral: never stored in state or plan."
  type = object({
    app_id          = string
    installation_id = string
    private_key     = string
  })
  sensitive = true
  ephemeral = true
}

variable "repo_secret_revision" {
  description = "Increment to re-write the repo credential (write-only values aren't diffed)."
  type        = number
  default     = 1
}

variable "target_revision" {
  description = "Git branch, tag or commit the root app tracks."
  type        = string
}

variable "root_app_path" {
  description = "Repo path holding the environment's child Applications."
  type        = string
}
