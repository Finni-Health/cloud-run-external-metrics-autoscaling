
resource "google_cloud_run_v2_service" "crema" {
  name     = var.service_name
  location = var.region
  project  = var.project_id

  labels = {
    "created-by" = "crema"
  }

  deletion_protection = var.deletion_protection

  template {
    service_account = var.service_account_email

    scaling {
      min_instance_count = 1
      max_instance_count = 1
    }

    containers {
      image = var.image

      # Base image for automatic base image updates
      base_image_uri = var.base_image

      resources {
        # no-cpu-throttling
        cpu_idle = false
      }

      env {
        name  = "CREMA_CONFIG"
        value = var.crema_config
      }

      env {
        name  = "OUTPUT_SCALER_METRICS"
        value = var.output_scaler_metrics ? "True" : "False"
      }

      env {
        name  = "ENABLE_CLOUD_LOGGING"
        value = var.enable_cloud_logging ? "True" : "False"
      }

      dynamic "env" {
        for_each = var.log_format != "" ? [1] : []
        content {
          name  = "LOG_FORMAT"
          value = var.log_format
        }
      }
    }
  }
}

resource "google_project_iam_member" "metric_writer" {
  count   = var.output_scaler_metrics ? 1 : 0
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${var.service_account_email}"
}

resource "google_project_iam_member" "log_writer" {
  count   = var.enable_cloud_logging ? 1 : 0
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${var.service_account_email}"
}
