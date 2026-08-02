import * as THREE from 'three';
import { EffectComposer } from 'three/addons/postprocessing/EffectComposer.js';
import { RenderPass } from 'three/addons/postprocessing/RenderPass.js';
import { UnrealBloomPass } from 'three/addons/postprocessing/UnrealBloomPass.js';

// --- OPTIMIZACIÓN MÓVIL ---
// Detectamos si es móvil para reducir calidad
const isMobile = /Android|webOS|iPhone|iPad|iPod|BlackBerry|IEMobile|Opera Mini/i.test(navigator.userAgent) || window.innerWidth < 768;

const canvas = document.getElementById('aura-orb-canvas');
const playButton = document.getElementById('play-button');
const container = document.getElementById('home');

if (canvas && playButton && container) {
  const audioClips = ['/audio/clip1.mp3', '/audio/clip2.mp3', '/audio/clip3.mp3'];
  let currentAudioIndex = 0;
  let isPlaying = false;
  let animationId; // Para poder cancelar la animación

  const neonPaletteHex = [
    '#52A4FF', '#F8E000', '#30f0c0', '#BA55C7', '#F3407C', '#A5093D', '#6A20FF', 
    '#9A6AFF', '#F85555', '#D20707', '#FFEE62', '#0079FB', '#3C52E7', '#F92C85', '#FB6340'
  ];
  const neonPalette = neonPaletteHex.map(hex => new THREE.Color(hex));

  let camera, renderer, scene, composer, bloomPass, orb, audioContext, analyserNode, audioSource, audioElement;
  let mouse = new THREE.Vector2(0, 0);
  let targetOrbPosition = new THREE.Vector3(0, 0, 0);

  function init() {
    audioContext = new AudioContext();
    analyserNode = audioContext.createAnalyser();
    analyserNode.fftSize = 256;
    audioElement = new Audio(audioClips[currentAudioIndex]);
    audioElement.addEventListener('ended', playNextAudio);
    audioSource = audioContext.createMediaElementSource(audioElement);
    audioSource.connect(analyserNode);
    analyserNode.connect(audioContext.destination);

    scene = new THREE.Scene();
    scene.background = new THREE.Color(0x000000); // Fondo negro sólido es más rápido
    camera = new THREE.PerspectiveCamera(75, container.clientWidth / container.clientHeight, 0.1, 1000);
    camera.position.set(0, 0, 2.5);

    renderer = new THREE.WebGLRenderer({ 
        canvas: canvas, 
        antialias: !isMobile,
        powerPreference: "high-performance" 
    });
    
    const pixelRatio = isMobile ? Math.min(window.devicePixelRatio, 1.5) : Math.min(window.devicePixelRatio, 2);
    renderer.setPixelRatio(pixelRatio);
    renderer.setSize(container.clientWidth, container.clientHeight);
    renderer.toneMapping = THREE.ACESFilmicToneMapping;

    const renderScene = new RenderPass(scene, camera);
    
    const bloomResolution = isMobile ? new THREE.Vector2(container.clientWidth / 2, container.clientHeight / 2) : new THREE.Vector2(container.clientWidth, container.clientHeight);
    
    bloomPass = new UnrealBloomPass(bloomResolution, 1.2, 0.4, 0.85);
    bloomPass.threshold = 0;
    bloomPass.strength = isMobile ? 0.8 : 1.0;
    bloomPass.radius = 0.5;

    composer = new EffectComposer(renderer);
    composer.addPass(renderScene);
    composer.addPass(bloomPass);

    // Reducir geometría en móviles (menos vértices que calcular)
    const geometrySegments = isMobile ? 64 : 100;
    const orbGeometry = new THREE.SphereGeometry(1.0, geometrySegments, geometrySegments);
    
    const orbMaterial = new THREE.ShaderMaterial({
      uniforms: {
        time: { value: 0 },
        audioLevel: { value: 0 },
        uColors: { value: neonPalette },
      },
      // ... (Shaders se mantienen igual, son eficientes en GPU)
      vertexShader: `
        uniform float time;
        uniform float audioLevel;
        varying vec3 vNormal;
        varying vec3 vWorldPosition;

        vec3 mod289(vec3 x) { return x - floor(x * (1.0 / 289.0)) * 289.0; }
        vec4 mod289(vec4 x) { return x - floor(x * (1.0 / 289.0)) * 289.0; }
        vec4 permute(vec4 x) { return mod289(((x*34.0)+1.0)*x); }
        vec4 taylorInvSqrt(vec4 r) { return 3.79284291400159 - 0.85373472095314 * r; }
        float snoise(vec3 v) {
            const vec2 C = vec2(1.0/6.0, 1.0/3.0); const vec4 D = vec4(0.0, 0.5, 1.0, 2.0);
            vec3 i = floor(v + dot(v, C.yyy)); vec3 x0 = v - i + dot(i, C.xxx);
            vec3 g = step(x0.yzx, x0.xyz); vec3 l = 1.0 - g; vec3 i1 = min(g.xyz, l.zxy); vec3 i2 = max(g.xyz, l.zxy);
            vec3 x1 = x0 - i1 + C.xxx; vec3 x2 = x0 - i2 + C.yyy; vec3 x3 = x0 - D.yyy;
            i = mod289(i);
            vec4 p = permute(permute(permute(i.z + vec4(0.0, i1.z, i2.z, 1.0)) + i.y + vec4(0.0, i1.y, i2.y, 1.0)) + i.x + vec4(0.0, i1.x, i2.x, 1.0));
            float n_ = 0.142857142857; vec3 ns = n_ * D.wyz - D.xzx;
            vec4 j = p - 49.0 * floor(p * ns.z * ns.z);
            vec4 x_ = floor(j * ns.z); vec4 y_ = floor(j - 7.0 * x_);
            vec4 x = x_ * ns.x + ns.yyyy; vec4 y = y_ * ns.x + ns.yyyy;
            vec4 h = 1.0 - abs(x) - abs(y);
            vec4 b0 = vec4(x.xy, y.xy); vec4 b1 = vec4(x.zw, y.zw);
            vec4 s0 = floor(b0)*2.0 + 1.0; vec4 s1 = floor(b1)*2.0 + 1.0; vec4 sh = -step(h, vec4(0.0));
            vec4 a0 = b0.xzyw + s0.xzyw*sh.xxyy; vec4 a1 = b1.xzyw + s1.xzyw*sh.zzww;
            vec3 p0 = vec3(a0.xy,h.x); vec3 p1 = vec3(a0.zw,h.y); vec3 p2 = vec3(a1.xy,h.z); vec3 p3 = vec3(a1.zw,h.w);
            vec4 norm = taylorInvSqrt(vec4(dot(p0,p0), dot(p1,p1), dot(p2,p2), dot(p3,p3)));
            p0 *= norm.x; p1 *= norm.y; p2 *= norm.z; p3 *= norm.w;
            vec4 m = max(0.6 - vec4(dot(x0,x0), dot(x1,x1), dot(x2,x2), dot(x3,x3)), 0.0);
            m = m * m;
            return 42.0 * dot(m*m, vec4(dot(p0,x0), dot(p1,x1), dot(p2,x2), dot(p3,x3)));
        }
        float fbm(vec3 p) {
            float value = 0.0;
            float amplitude = 0.5;
            for (int i = 0; i < 3; i++) {
                value += amplitude * snoise(p);
                p *= 2.0;
                amplitude *= 0.5;
            }
            return value;
        }
        void main() {
          float baseDisplacement = fbm(normal * 2.0 + time * 0.5) * 0.05;
          float audioDisplacement = fbm(normal * 4.0 + time * 1.0) * audioLevel * 0.3;
          float totalDisplacement = baseDisplacement + audioDisplacement;
          vec3 deformedPosition = position + normal * totalDisplacement;
          vNormal = normalize(normalMatrix * normal);
          vec4 worldPosition = modelMatrix * vec4(deformedPosition, 1.0);
          vWorldPosition = worldPosition.xyz;
          gl_Position = projectionMatrix * viewMatrix * worldPosition;
        }
      `,
      fragmentShader: `
        uniform float time;
        uniform float audioLevel;
        uniform vec3 uColors[15];
        varying vec3 vNormal;
        varying vec3 vWorldPosition;

        vec3 mod289(vec3 x) { return x - floor(x * (1.0 / 289.0)) * 289.0; }
        vec4 mod289(vec4 x) { return x - floor(x * (1.0 / 289.0)) * 289.0; }
        vec4 permute(vec4 x) { return mod289(((x*34.0)+1.0)*x); }
        vec4 taylorInvSqrt(vec4 r) { return 1.79284291400159 - 0.85373472095314 * r; }
        float snoise(vec3 v) {
            const vec2 C = vec2(1.0/6.0, 1.0/3.0); const vec4 D = vec4(0.0, 0.5, 1.0, 2.0);
            vec3 i = floor(v + dot(v, C.yyy)); vec3 x0 = v - i + dot(i, C.xxx);
            vec3 g = step(x0.yzx, x0.xyz); vec3 l = 1.0 - g; vec3 i1 = min(g.xyz, l.zxy); vec3 i2 = max(g.xyz, l.zxy);
            vec3 x1 = x0 - i1 + C.xxx; vec3 x2 = x0 - i2 + C.yyy; vec3 x3 = x0 - D.yyy;
            i = mod289(i);
            vec4 p = permute(permute(permute(i.z + vec4(0.0, i1.z, i2.z, 1.0)) + i.y + vec4(0.0, i1.y, i2.y, 1.0)) + i.x + vec4(0.0, i1.x, i2.x, 1.0));
            float n_ = 0.142857142857; vec3 ns = n_ * D.wyz - D.xzx;
            vec4 j = p - 49.0 * floor(p * ns.z * ns.z);
            vec4 x_ = floor(j * ns.z); vec4 y_ = floor(j - 7.0 * x_);
            vec4 x = x_ * ns.x + ns.yyyy; vec4 y = y_ * ns.x + ns.yyyy;
            vec4 h = 1.0 - abs(x) - abs(y);
            vec4 b0 = vec4(x.xy, y.xy); vec4 b1 = vec4(x.zw, y.zw);
            vec4 s0 = floor(b0)*2.0 + 1.0; vec4 s1 = floor(b1)*2.0 + 1.0; vec4 sh = -step(h, vec4(0.0));
            vec4 a0 = b0.xzyw + s0.xzyw*sh.xxyy; vec4 a1 = b1.xzyw + s1.xzyw*sh.zzww;
            vec3 p0 = vec3(a0.xy,h.x); vec3 p1 = vec3(a0.zw,h.y); vec3 p2 = vec3(a1.xy,h.z); vec3 p3 = vec3(a1.zw,h.w);
            vec4 norm = taylorInvSqrt(vec4(dot(p0,p0), dot(p1,p1), dot(p2,p2), dot(p3,p3)));
            p0 *= norm.x; p1 *= norm.y; p2 *= norm.z; p3 *= norm.w;
            vec4 m = max(0.6 - vec4(dot(x0,x0), dot(x1,x1), dot(x2,x2), dot(x3,x3)), 0.0);
            m = m * m;
            return 42.0 * dot(m*m, vec4(dot(p0,x0), dot(p1,x1), dot(p2,x2), dot(p3,x3)));
        }
        float fbm(vec3 p) {
            float value = 0.0;
            float amplitude = 0.5;
            for (int i = 0; i < 3; i++) {
                value += amplitude * snoise(p);
                p *= 2.0;
                amplitude *= 0.5;
            }
            return value;
        }

        void main() {
            vec3 viewDirection = normalize(cameraPosition - vWorldPosition);
            float fresnel = 1.0 - dot(vNormal, viewDirection);
            fresnel = pow(fresnel, 2.0);

            float noise = fbm(vNormal * 3.0 + time * 0.2);
            float energyNoise = fbm(vNormal * 6.0 + time * 0.5);
            float energyFilaments = pow(abs(energyNoise), 8.0);
            
            float alpha = fresnel * (0.3 + noise * 0.7);
            alpha += energyFilaments * (0.5 + audioLevel * 3.0);
            
            float colorIndex = mod((vWorldPosition.y + time * 0.1) * 5.0, 14.0);
            int index1 = int(floor(colorIndex));
            int index2 = int(ceil(colorIndex));
            vec3 color1 = uColors[index1];
            vec3 color2 = uColors[index2];
            float mixFactor = fract(colorIndex);
            vec3 color = mix(color1, color2, mixFactor);
            
            vec3 finalColor = color + vec3(energyFilaments * (0.8 + audioLevel * 8.0));

            gl_FragColor = vec4(finalColor, alpha);
        }
      `,
      transparent: true,
      blending: THREE.AdditiveBlending,
      depthWrite: false,
    });
    
    orb = new THREE.Mesh(orbGeometry, orbMaterial);
    scene.add(orb);
    
    const onResize = () => {
      const width = container.clientWidth;
      const height = container.clientHeight;
      if (width === 0 || height === 0) return;
      camera.aspect = width / height;
      camera.updateProjectionMatrix();
      renderer.setSize(width, height);
      composer.setSize(width, height);
    }
    const resizeObserver = new ResizeObserver(onResize);
    resizeObserver.observe(container);

    container.addEventListener('mousemove', onPointerMove);
    container.addEventListener('touchmove', onPointerMove);

    // --- CRÍTICO: DETENER RENDER SI NO ES VISIBLE ---
    // Esto salva la batería y CPU cuando scrolleas hacia abajo
    const intersectionObserver = new IntersectionObserver((entries) => {
        entries.forEach(entry => {
            if (entry.isIntersecting) {
                // Si el héroe es visible, iniciamos el loop
                if (!animationId) animate();
            } else {
                // Si el héroe NO es visible, cancelamos el loop
                if (animationId) {
                    cancelAnimationFrame(animationId);
                    animationId = null;
                }
            }
        });
    });
    intersectionObserver.observe(container);
  }
  
  function animate() {
    animationId = requestAnimationFrame(animate);
    
    const bufferLength = analyserNode.frequencyBinCount;
    const dataArray = new Uint8Array(bufferLength);
    analyserNode.getByteFrequencyData(dataArray);

    const audioLevel = dataArray.reduce((sum, value) => sum + value, 0) / (bufferLength * 350);
    
    const t = performance.now();
    const material = orb.material;
    material.uniforms.time.value = t * 0.0003;
    material.uniforms.audioLevel.value = audioLevel;
    
    orb.position.lerp(targetOrbPosition, 0.05);
    
    bloomPass.strength = isMobile ? (8.0 + audioLevel * 8.0) : (9.0 + audioLevel * 10.0);
    orb.rotation.y += 0.0005;

    composer.render();
  }

  function playNextAudio() {
    setTimeout(() => {
        currentAudioIndex = (currentAudioIndex + 1) % audioClips.length;
        audioElement.src = audioClips[currentAudioIndex];
        audioElement.play();
        isPlaying = true;
    }, 1500);
  }

  function onPointerMove(event) {
    // Reducir frecuencia de eventos si es necesario, pero en este caso es ligero
    let x, y;
    if (event.touches) {
      x = event.touches[0].clientX;
      y = event.touches[0].clientY;
    } else {
      x = event.clientX;
      y = event.clientY;
    }
    
    mouse.x = (x / window.innerWidth) * 2 - 1;
    mouse.y = - (y / window.innerHeight) * 2 + 1;

    targetOrbPosition.x = mouse.x * 0.5;
    targetOrbPosition.y = mouse.y * 0.5;
  }
  
  playButton.addEventListener('click', () => {
    if (audioContext.state === 'suspended') {
      audioContext.resume();
    }
    
    if (isPlaying) {
      audioElement.pause();
      isPlaying = false;
      playButton.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="currentColor" class="w-4 h-4 mr-2"><path d="M8 5v14l11-7z"></path></svg> Play`;
    } else {
      audioElement.play();
      isPlaying = true;
      playButton.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="currentColor" class="w-4 h-4 mr-2"><path d="M6 6h12v12H6z"></path></svg> Stop`;
    }
  });
  
  init();
}