// Research-only blind forwarding to one explicitly owned HTTPS relay.
// Keep the private URL/secret/expiry out of tracked source. This service
// requires a separately approved script.external_request authorization.
const RELAY_URL = "https://REPLACE_WITH_OWNED_ENDPOINT/exchange";
const SCRIPT_SECRET = "REPLACE_IN_PRIVATE_COPY";
const RELAY_HTTP_AUTH = "REPLACE_WITH_DISTINCT_PRIVATE_SECRET";
const RELAY_EXPIRES_UNIX_MS = 0;
const RELAY_BODY_LIMIT = 6000000;

function doPost(event) {
  const reject = () => ContentService.createTextOutput('{"error":"rejected"}')
    .setMimeType(ContentService.MimeType.JSON);
  if (Date.now() >= RELAY_EXPIRES_UNIX_MS || !event || !event.postData ||
      event.postData.length > RELAY_BODY_LIMIT) return reject();
  try {
    const request = JSON.parse(event.postData.contents);
    if (request.auth !== SCRIPT_SECRET || typeof request.data !== "string" ||
        request.data.length > RELAY_BODY_LIMIT || !/^[A-Za-z0-9+/]*={0,2}$/.test(request.data)) return reject();
    const response = UrlFetchApp.fetch(RELAY_URL, {
      method: "post", contentType: "application/json",
      headers: {Authorization: "Bearer " + RELAY_HTTP_AUTH},
      payload: JSON.stringify({auth: RELAY_HTTP_AUTH, data: request.data}),
      followRedirects: false, muteHttpExceptions: true,
      validateHttpsCertificates: true, timeoutSeconds: 25
    });
    if (response.getResponseCode() !== 200) return reject();
    const body = response.getContentText();
    if (body.length > RELAY_BODY_LIMIT) return reject();
    return ContentService.createTextOutput(body).setMimeType(ContentService.MimeType.JSON);
  } catch (_) { return reject(); }
}
