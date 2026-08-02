
# Deep Axiom Aura - Website

Este repositorio contiene el código fuente del sitio web construido con [Astro](https://astro.build/) y configurado para ser alojado en **Firebase Hosting**.

---

## 🚀 Cómo empezar en local

1. **Instalar dependencias:**
   Asegúrate de estar en la raíz del proyecto y ejecuta:
   ```bash
   npm install
   ```

2. **Ejecutar el servidor de desarrollo:**
   ```bash
   npm run dev
   ```
   El sitio estará disponible localmente, generalmente en `http://localhost:4321`.

---

## ☁️ Cómo Desplegar el Sitio

Para subir los últimos cambios a Firebase, sigue estos pasos:

1. **Construye el proyecto (Build):**
   Esto genera los archivos estáticos finales en la carpeta `dist/`.
   ```bash
   npm run build
   ```

2. **Despliega en Firebase Hosting:**
   *(Asegúrate de haber iniciado sesión previamente con `firebase login`)*
   ```bash
   firebase deploy --only hosting
   ```

---

## 🔄 Cómo mover el proyecto a otro entorno de Firebase

Si deseas usar esta base de código para otro cliente, entorno (desarrollo/producción) o moverlo a un proyecto de Firebase completamente nuevo, necesitas actualizar los archivos de configuración para que apunten al lugar correcto.

Aquí te explicamos qué cambiar:

### 1. `.firebaserc` (Conexión con el Proyecto principal)
Este archivo vincula tu carpeta local con un **Proyecto de Firebase** específico en la nube (por ejemplo, `aegis-ai-core`).

Para cambiar a otro proyecto, abre el archivo `.firebaserc` y actualiza el valor de `"default"` con el **ID de tu nuevo proyecto**:

```json
{
  "projects": {
    "default": "NUEVO-ID-DEL-PROYECTO"
  }
}
```

### 2. `firebase.json` (Conexión con el Sitio de Hosting)
Dentro de un proyecto de Firebase puedes tener múltiples sitios web alojados. Si tu sitio no usa el nombre por defecto del proyecto, Firebase necesita saber exactamente a cuál "sub-sitio" enviar los archivos.

Abre el archivo `firebase.json`. Si quieres apuntar a un sitio específico (por ejemplo, `mi-nuevo-sitio`), modifica el atributo `"site"`:

```json
{
  "hosting": {
    "site": "nombre-de-tu-nuevo-sitio", 
    "public": "dist",
    "ignore": [
      "firebase.json",
      "**/.*",
      "**/node_modules/**"
    ]
  }
}
```
> **💡 Tip:** Si quieres que Firebase asuma automáticamente el sitio por defecto (que tiene el mismo nombre que el ID del proyecto en `.firebaserc`), simplemente **elimina la línea `"site": "..."`** por completo.

### 3. `package.json` (Dependencias y Scripts)
Este archivo maneja la configuración de Node.js. Si estás moviendo el proyecto, no necesitas cambiar nada de la configuración de Firebase aquí, pero es vital asegurar que los comandos de compilación sigan intactos. 

Tu bloque de scripts debe verse similar a esto para que el comando `npm run build` que usamos antes funcione correctamente y genere la carpeta `dist` que Firebase lee:

```json
{
  "name": "deepaxiom-aura-website",
  "version": "0.0.1",
  "scripts": {
    "dev": "astro dev",
    "start": "astro dev",
    "build": "astro build",
    "preview": "astro preview"
  },
  "dependencies": {
    "astro": "^4.0.0"
  }
}
```
