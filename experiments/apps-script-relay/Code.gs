// Research-only blind forwarding to one explicitly owned HTTPS relay.
// Keep the private URL/secret/expiry out of tracked source. This service
// requires a separately approved script.external_request authorization.
const RELAY_URL = "https://REPLACE_WITH_OWNED_ENDPOINT/exchange";
const SCRIPT_SECRET = "REPLACE_IN_PRIVATE_COPY";
const RELAY_HTTP_AUTH = "REPLACE_WITH_DISTINCT_PRIVATE_SECRET";
const RELAY_EXPIRES_UNIX_MS = 0;
const RELAY_BODY_LIMIT = 6000000;

function doPost(event) {
  const reject = (code) => ContentService.createTextOutput(JSON.stringify({error: code}))
    .setMimeType(ContentService.MimeType.JSON);
  if (Date.now() >= RELAY_EXPIRES_UNIX_MS) return reject("expired");
  if (!event || !event.postData || event.postData.length > RELAY_BODY_LIMIT) return reject("request_bound");
  try {
    let request;
    try { request = JSON.parse(event.postData.contents); } catch (_) { return reject("request_shape"); }
    if (!request || request.auth !== SCRIPT_SECRET) return reject("auth");
    if (typeof request.data !== "string" || request.data.length > RELAY_BODY_LIMIT ||
        !/^[A-Za-z0-9+/]*={0,2}$/.test(request.data)) return reject("request_shape");
    const response = UrlFetchApp.fetch(RELAY_URL, {
      method: "post", contentType: "application/json",
      headers: {Authorization: "Bearer " + RELAY_HTTP_AUTH},
      payload: JSON.stringify({auth: RELAY_HTTP_AUTH, data: request.data}),
      followRedirects: false, muteHttpExceptions: true,
      validateHttpsCertificates: true, timeoutSeconds: 25
    });
    if (response.getResponseCode() !== 200) return reject("relay_http_" + response.getResponseCode());
    const body = response.getContentText();
    if (body.length > RELAY_BODY_LIMIT) return reject("response_bound");
    return ContentService.createTextOutput(body).setMimeType(ContentService.MimeType.JSON);
  } catch (_) { return reject("fetch_failed"); }
}
