{{- define "modeldock.fullname" -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "modeldock.labels" -}}
app.kubernetes.io/name: modeldock
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "modeldock.secretName" -}}
{{- default (printf "%s-secrets" (include "modeldock.fullname" .)) .Values.existingSecret -}}
{{- end -}}

{{- define "modeldock.env" -}}
- name: MODELDOCK_DATABASE_URL
  valueFrom: { secretKeyRef: { name: {{ include "modeldock.secretName" . }}, key: databaseUrl } }
- name: MODELDOCK_JWT_SECRET
  valueFrom: { secretKeyRef: { name: {{ include "modeldock.secretName" . }}, key: jwtSecret } }
- name: MODELDOCK_KAFKA_BROKERS
  value: {{ .Values.config.kafkaBrokers | quote }}
{{- end -}}
