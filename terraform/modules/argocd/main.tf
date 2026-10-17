# Terraform's last Kubernetes responsibility: install Argo CD, give it read
# access to the repo, and create the single root Application. Everything the
# root app points at is owned by Argo CD, not Terraform.

resource "kubernetes_namespace_v1" "argocd" {
  metadata {
    name = var.namespace
  }
}

# Declarative repository credential using a read-only GitHub App: Argo CD
# mints installation tokens that expire after an hour instead of sending a
# static credential. The app credentials arrive as an ephemeral value and are
# written through a write-only attribute, so they never enter state.
# Bump repo_secret_revision to push rotated credentials.
resource "kubernetes_secret_v1" "repo" {
  metadata {
    name      = "repo-${replace(lower(var.repo_name), "/[^a-z0-9-]/", "-")}"
    namespace = kubernetes_namespace_v1.argocd.metadata[0].name
    labels = {
      "argocd.argoproj.io/secret-type" = "repository"
    }
  }

  data_wo = {
    type                    = "git"
    url                     = var.repo_url
    githubAppID             = var.github_app.app_id
    githubAppInstallationID = var.github_app.installation_id
    githubAppPrivateKey     = var.github_app.private_key
  }
  data_wo_revision = var.repo_secret_revision
}

resource "helm_release" "argocd" {
  name       = "argocd"
  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argo-cd"
  version    = var.chart_version
  namespace  = kubernetes_namespace_v1.argocd.metadata[0].name

  wait    = true
  timeout = 600

  values = [yamlencode({
    # Single-replica, non-HA install sized for two t3.medium nodes. The UI/API
    # stays ClusterIP; reach it with `kubectl port-forward`.
    dex = { enabled = false }
    server = {
      service = { type = "ClusterIP" }
    }
  })]

  depends_on = [kubernetes_secret_v1.repo]
}

# Installed through the argocd-apps chart rather than kubernetes_manifest:
# the Application CRD doesn't exist until Argo CD is installed, and
# kubernetes_manifest needs it at plan time.
resource "helm_release" "root_app" {
  name       = "root"
  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argocd-apps"
  version    = var.apps_chart_version
  namespace  = kubernetes_namespace_v1.argocd.metadata[0].name

  values = [yamlencode({
    applications = {
      root = {
        namespace = var.namespace
        # Deleting the root app cascades to everything it created, so
        # `terraform destroy` removes Argo-managed resources first.
        finalizers = ["resources-finalizer.argocd.argoproj.io"]
        project    = "default"
        source = {
          repoURL        = var.repo_url
          targetRevision = var.target_revision
          path           = var.root_app_path
        }
        destination = {
          server    = "https://kubernetes.default.svc"
          namespace = var.namespace
        }
        syncPolicy = {
          automated = {
            prune    = true
            selfHeal = true
          }
        }
      }
    }
  })]

  depends_on = [helm_release.argocd]
}
