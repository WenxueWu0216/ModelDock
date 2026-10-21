# Sample model server. The model is trained at build time and baked in, so
# the image is immutable: one image digest == one model version.
ARG BASE_IMAGE=python:3.12-slim
FROM ${BASE_IMAGE} AS build
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir --prefix=/install -r requirements.txt
COPY train.py .
RUN PYTHONPATH=$(echo /install/lib/python3.12/site-packages) python train.py --out /app/model.joblib

FROM ${BASE_IMAGE}
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 MODEL_PATH=/app/model.joblib PORT=8080
WORKDIR /app
COPY --from=build /install /usr/local
COPY --from=build /app/model.joblib /app/model.joblib
COPY app ./app
USER 10001:10001
EXPOSE 8080
CMD ["sh", "-c", "exec uvicorn app.main:app --host 0.0.0.0 --port ${PORT} --no-access-log"]
