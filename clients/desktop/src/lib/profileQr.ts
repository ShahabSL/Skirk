import { encodeQR } from "qr";

const maxQrProfileBytes = 2953;
const maxImageBytes = 16 * 1024 * 1024;
const maxScanDimension = 2400;

export const qrImageTypes = "image/png,image/jpeg,image/webp";

export function createProfileQr(profile: string): string | null {
  if (profile.length > maxQrProfileBytes) return null;
  const svg = encodeQR(profile, "svg", { ecc: "low", encoding: "byte", border: 4 });
  const crisp = svg.replace("<svg ", '<svg shape-rendering="crispEdges" ');
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(crisp)}`;
}

export async function readProfileQr(file: File): Promise<string> {
  if (file.size > maxImageBytes) {
    throw new Error("This image is larger than 16 MiB. Crop it to the QR code and try again.");
  }
  if (!qrImageTypes.split(",").includes(file.type)) {
    throw new Error("Choose a PNG, JPEG, or WebP image.");
  }
  const image = await loadQrImage(file).catch(() => {
    throw new Error("Could not open this image. Try another PNG, JPEG, or WebP file.");
  });
  try {
    const scale = Math.min(1, maxScanDimension / Math.max(image.width, image.height));
    const canvas = document.createElement("canvas");
    canvas.width = Math.max(1, Math.floor(image.width * scale));
    canvas.height = Math.max(1, Math.floor(image.height * scale));
    const context = canvas.getContext("2d");
    if (!context) {
      throw new Error("Could not read image pixels. You can paste the profile instead.");
    }
    context.fillStyle = "white";
    context.fillRect(0, 0, canvas.width, canvas.height);
    context.drawImage(image, 0, 0, canvas.width, canvas.height);
    const pixels = context.getImageData(0, 0, canvas.width, canvas.height);
    const { decodeQR } = await import("qr/decode.js");
    let profile: string;
    try {
      profile = decodeQR(pixels).trim();
    } catch {
      throw new Error("No QR code found. Use a sharp image showing the whole code.");
    }
    if (!/^skirk:[A-Za-z0-9_-]+$/.test(profile)) {
      throw new Error("This QR code does not contain a Skirk profile.");
    }
    return profile;
  } finally {
    if ("close" in image) image.close();
  }
}

async function loadQrImage(file: File): Promise<ImageBitmap | HTMLImageElement> {
  if (typeof createImageBitmap === "function") return createImageBitmap(file);
  // Older webviews can still decode a local image through the allowed data URL.
  const data = await new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      if (typeof reader.result === "string") resolve(reader.result);
      else reject(new Error("Image file is unavailable."));
    };
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(file);
  });
  const image = new Image();
  image.src = data;
  await image.decode();
  return image;
}
