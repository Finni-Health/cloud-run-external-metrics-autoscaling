
provider "google" {
  project = var.project_id
  region  = var.region
}

# 1. Create the Service Account for CREMA
resource "google_service_account" "crema_sa" {
  account_id   = "crema-example-sa"
  display_name = "CREMA Example Service Account"
}

# 2. Grant necessary permissions to the CREMA Service Account
resource "google_project_iam_member" "crema_sa_permissions" {
  for_each = toset([
    "roles/parametermanager.parameterViewer",
    "roles/run.developer",
    "roles/iam.serviceAccountUser"
  ])

  project = var.project_id
  role    = each.key
  member  = "serviceAccount:${google_service_account.crema_sa.email}"
}

# 3. Create Parameter Manager parameter for CREMA configuration
resource "google_parameter_manager_parameter" "crema_config" {
  parameter_id = "crema-example-config"
  format       = "YAML"
}

# 4. Create a target Cloud Run worker pool for CREMA to scale
resource "google_cloud_run_v2_worker_pool" "target_worker_pool" {
  name                = "target-worker-pool"
  location            = var.region
  project             = var.project_id
  deletion_protection = false

  template {
    containers {
      image = "us-docker.pkg.dev/cloudrun/container/worker-pool:latest"
    }
  }

  scaling {
    scaling_mode          = "MANUAL"
    manual_instance_count = 0
  }

  lifecycle {
    ignore_changes = [
      scaling[0].manual_instance_count
    ]
  }
}

# 5. Add a valid CREMA configuration as a version
resource "google_parameter_manager_parameter_version" "crema_config_version" {
  parameter            = google_parameter_manager_parameter.crema_config.id
  parameter_version_id = "1"
  parameter_data       = <<EOF
apiVersion: crema/v1
kind: CremaConfig
spec:
  pollingInterval: 15
  scaledObjects:
    - spec:
        scaleTargetRef:
          name: projects/${var.project_id}/locations/${var.region}/workerpools/target-worker-pool
        minReplicaCount: 0
        maxReplicaCount: 5
        triggers:
          - type: cron
            name: cron-example
            metadata:
              timezone: UTC
              start: "0 0 * * *"
              end: "59 23 * * *"
              desiredReplicas: "3"
EOF
  depends_on = [google_cloud_run_v2_worker_pool.target_worker_pool]
}

# 6. Wait for IAM permissions to propagate
resource "time_sleep" "wait_for_iam" {
  depends_on = [google_project_iam_member.crema_sa_permissions]

  create_duration = "45s"

  triggers = {
    service_account = google_service_account.crema_sa.id
  }
}

# 7. Deploy CREMA using the module
module "crema" {
  source = "../.."

  project_id            = var.project_id
  region                = var.region
  service_account_email = google_service_account.crema_sa.email
  crema_config          = google_parameter_manager_parameter_version.crema_config_version.id

  service_name          = "crema-example"
  deletion_protection   = false

  depends_on = [time_sleep.wait_for_iam]
}
