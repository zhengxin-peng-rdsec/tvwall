package com.tvwall.client;

import android.app.Activity;
import android.graphics.Bitmap;
import android.graphics.Rect;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.provider.Settings;
import android.view.PixelCopy;
import android.view.View;
import android.webkit.CookieManager;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.URL;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public class MainActivity extends Activity {
    private WebView webView;
    private final Handler main = new Handler(Looper.getMainLooper());
    private final ExecutorService io = Executors.newSingleThreadExecutor();
    private String deviceId, token;
    private volatile boolean ready;
    private volatile int refreshSeconds = 5;
    private final Runnable pollTask = new Runnable() { public void run() { poll(); main.postDelayed(this, 3000); } };
    private final Runnable frameTask = new Runnable() { public void run() { if (ready) capture(false); main.postDelayed(this, refreshSeconds * 1000L); } };

    @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        getWindow().setFlags(0x1000000, 0x1000000); // FLAG_HARDWARE_ACCELERATED
        getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        getWindow().getDecorView().setSystemUiVisibility(5894 | View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY);
        deviceId = Settings.Secure.getString(getContentResolver(), Settings.Secure.ANDROID_ID);
        if (deviceId == null) deviceId = "android-" + System.currentTimeMillis();
        token = getPreferences(0).getString("token", "");
        webView = new WebView(this); webView.setBackgroundColor(0xff101417);
        WebSettings s = webView.getSettings(); s.setJavaScriptEnabled(true); s.setDomStorageEnabled(true); s.setMediaPlaybackRequiresUserGesture(false); s.setAllowFileAccess(false); s.setCacheMode(WebSettings.LOAD_DEFAULT);
        webView.setLayerType(View.LAYER_TYPE_HARDWARE, null); webView.setWebViewClient(new WebViewClient()); webView.setWebChromeClient(new WebChromeClient()); CookieManager.getInstance().setAcceptCookie(true);
        setContentView(new FrameLayout(this) {{ addView(webView, new FrameLayout.LayoutParams(-1, -1)); }});
        new DiscoveryServer(this).start();
        main.post(pollTask); main.postDelayed(frameTask, 5000);
    }

    private void poll() { io.execute(() -> { try {
        JSONObject body = new JSONObject().put("id", deviceId).put("name", android.os.Build.MODEL).put("token", token);
        JSONObject reg = request("/api/client/register", "POST", body.toString());
        token = reg.optString("token", token); getPreferences(0).edit().putString("token", token).apply();
        applyConfig(reg); JSONArray commands = reg.optJSONArray("commands"); handleCommands(commands);
        JSONObject p = request("/api/client/poll?device_id=" + enc(deviceId) + "&token=" + enc(token), "GET", null);
        applyConfig(p); handleCommands(p.optJSONArray("commands")); ready = true;
    } catch (Exception ignored) {} }); }

    private void applyConfig(JSONObject data) { String next = data.optString("url", ""); refreshSeconds = Math.max(3, data.optInt("refresh_seconds", 5)); if (!next.isEmpty()) main.post(() -> { if (!next.equals(webView.getUrl())) webView.loadUrl(next); }); }
    private void handleCommands(JSONArray a) { if (a == null) return; for (int i=0;i<a.length();i++) if ("screenshot".equals(a.optJSONObject(i).optString("type"))) main.post(() -> capture(true)); }
    private void capture(boolean immediate) { if (webView.getWidth() < 2 || webView.getHeight() < 2) return; Bitmap b = Bitmap.createBitmap(webView.getWidth(), webView.getHeight(), Bitmap.Config.ARGB_8888); Rect rect = new Rect(0,0,webView.getWidth(),webView.getHeight()); PixelCopy.request(getWindow(), rect, b, result -> { if (result == PixelCopy.SUCCESS) upload(b, immediate); else b.recycle(); }, main); }
    private void upload(Bitmap bitmap, boolean immediate) { io.execute(() -> { try { ByteArrayOutputStream out = new ByteArrayOutputStream(); bitmap.compress(Bitmap.CompressFormat.JPEG, immediate ? 82 : 68, out); bitmap.recycle(); JSONObject body = new JSONObject().put("data", android.util.Base64.encodeToString(out.toByteArray(), android.util.Base64.NO_WRAP)); request("/api/client/" + (immediate ? "screenshot" : "frame") + "?device_id=" + enc(deviceId) + "&token=" + enc(token), "POST", body.toString()); } catch (Exception ignored) {} }); }
    private JSONObject request(String path, String method, String body) throws Exception { URL u = new URL(BuildConfig.SERVER_URL + path); HttpURLConnection c = (HttpURLConnection)u.openConnection(); c.setConnectTimeout(4000); c.setReadTimeout(8000); c.setRequestMethod(method); c.setRequestProperty("Content-Type", "application/json"); if (body != null) { c.setDoOutput(true); try(OutputStream o=c.getOutputStream()){o.write(body.getBytes(StandardCharsets.UTF_8));} } try(InputStream in=c.getInputStream(); ByteArrayOutputStream out=new ByteArrayOutputStream()){ byte[] chunk=new byte[8192]; int count; while((count=in.read(chunk))!=-1) out.write(chunk,0,count); return new JSONObject(new String(out.toByteArray(), StandardCharsets.UTF_8)); } }
    private static String enc(String s) throws Exception { return URLEncoder.encode(s, "UTF-8"); }
    @Override protected void onDestroy() { main.removeCallbacksAndMessages(null); io.shutdownNow(); if (webView != null) webView.destroy(); super.onDestroy(); }

    static class DiscoveryServer extends Thread { private final MainActivity a; DiscoveryServer(MainActivity x){a=x;setDaemon(true);} public void run(){try(ServerSocket ss=new ServerSocket(17301)){while(true){try(Socket s=ss.accept()){byte[] buf=new byte[512];int n=s.getInputStream().read(buf);String req=new String(buf,0,Math.max(0,n),StandardCharsets.US_ASCII);if(req.contains("/playbridge-discovery")){String body=new JSONObject().put("id",a.deviceId).put("name",android.os.Build.MODEL).toString();String out="HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "+body.getBytes(StandardCharsets.UTF_8).length+"\r\nConnection: close\r\n\r\n"+body;s.getOutputStream().write(out.getBytes(StandardCharsets.UTF_8));}}}}catch(Exception ignored){}} }
}
