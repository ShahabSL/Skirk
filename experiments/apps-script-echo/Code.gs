// Temporary owned echo only: no destination access, Drive or Gmail service.
// Replace these placeholders only in the ignored private deployment copy.
const ECHO_SECRET = "REPLACE_IN_PRIVATE_COPY";
const ECHO_EXPIRES_UNIX_MS = 0;
const ECHO_BODY_LIMIT = 1500000;

function doPost(event) {
  const reject = () => ContentService.createTextOutput('{"ok":false}')
    .setMimeType(ContentService.MimeType.JSON);
  if (Date.now() >= ECHO_EXPIRES_UNIX_MS || !event || !event.postData ||
      event.postData.length > ECHO_BODY_LIMIT) return reject();
  try {
    const request = JSON.parse(event.postData.contents);
    if (request.auth !== ECHO_SECRET || typeof request.nonce !== "string" ||
        request.nonce.length !== 16 || typeof request.data !== "string" ||
        request.data.length > 1400000 || !/^[A-Za-z0-9+/]*={0,2}$/.test(request.data)) return reject();
    return ContentService.createTextOutput(JSON.stringify({
      ok: true, nonce: request.nonce, data: request.data
    })).setMimeType(ContentService.MimeType.JSON);
  } catch (_) {
    return reject();
  }
}
