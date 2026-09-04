# El lector de imagen, como contenedor.
#
# Una skill es un cliente del kernel: se conecta hacia afuera por WebSocket, con
# `AURA_WS_URL` y `AURA_TOKEN`. Por eso no vive dentro de la imagen del kernel
# --que es distroless y no tiene Python-- sino al lado, con su propio ciclo de
# vida: un lector que se cae no se lleva el gate por delante.
#
# El SDK se copia del repositorio y no de un registro porque todavia no esta
# publicado. Ver GUIDE.md#references.
FROM python:3.12-slim

WORKDIR /app

COPY sdk/python /sdk/python
COPY skills/imaging-read/requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY skills/imaging-read/ ./
ENV PYTHONPATH=/sdk/python/src
ENV PYTHONUNBUFFERED=1

# Sin privilegios. Lo unico que este proceso necesita leer es la credencial que
# se le monta, y montarla de solo lectura es la otra mitad de eso.
RUN useradd --create-home --uid 10001 reader
USER reader

CMD ["python", "main.py"]
