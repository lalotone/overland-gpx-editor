package com.wails.app;

import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.database.Cursor;
import android.net.Uri;
import android.os.CancellationSignal;
import android.provider.OpenableColumns;
import android.widget.Toast;

import org.json.JSONObject;

import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.Locale;
import java.util.UUID;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.Executors;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ScheduledFuture;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicReference;

/** Stages granted content URIs before their temporary read permission expires.
 * The authenticated Go host delivers these files once the React draft is ready.
 * A directory rename publishes the document and its display name together. */
public final class IncomingGpx {
    static final long MAX_BYTES = 16L * 1024 * 1024;
    private static final int MAX_PENDING = 8;
    private static final ThreadPoolExecutor WORKER = new ThreadPoolExecutor(
        1, 1, 0, TimeUnit.SECONDS, new ArrayBlockingQueue<>(4));
    private static final ScheduledExecutorService TIMER = Executors.newSingleThreadScheduledExecutor();

    private IncomingGpx() {}

    public static void receive(Activity activity, Intent intent) {
        if (intent == null) return;
        String action = intent.getAction();
        if (!Intent.ACTION_SEND.equals(action) && !Intent.ACTION_VIEW.equals(action)) return;
        // Consumed even if rejected: activity recreation must not replay it.
        activity.setIntent(new Intent(activity, MainActivity.class));
        Context context = activity.getApplicationContext();
        try {
            Uri uri = Intent.ACTION_VIEW.equals(action) ? intent.getData()
                : intent.getParcelableExtra(Intent.EXTRA_STREAM);
            if (uri == null && intent.getClipData() != null && intent.getClipData().getItemCount() == 1) {
                uri = intent.getClipData().getItemAt(0).getUri();
            }
            if (uri == null || !"content".equals(uri.getScheme()) ||
                (context.getPackageName() + ".fileprovider").equals(uri.getAuthority())) {
                throw new IOException("Share a GPX file attachment with read access.");
            }
            final Uri source = uri;
            final String mime = intent.getType();
            WORKER.execute(() -> {
                try {
                    stage(context, source, mime);
                } catch (Exception error) {
                    notice(context, "Could not open shared GPX: " + error.getMessage());
                }
            });
        } catch (RejectedExecutionException error) {
            notice(context, "Too many incoming files. Open pending GPX files first.");
        } catch (Exception error) {
            notice(context, "Could not open shared GPX: " + error.getMessage());
        }
    }

    private static void notice(Context context, String message) {
        new android.os.Handler(android.os.Looper.getMainLooper()).post(
            () -> Toast.makeText(context, message, Toast.LENGTH_LONG).show());
    }

    private static void stage(Context context, Uri uri, String mime) throws Exception {
        File inbox = new File(context.getFilesDir(), "overland/incoming");
        if (!inbox.isDirectory() && !inbox.mkdirs()) throw new IOException("Cannot prepare the import inbox.");
        File[] entries = inbox.listFiles();
        if (entries == null) throw new IOException("Cannot read the import inbox.");
        int pending = 0;
        for (File entry : entries) {
            // One copying worker: unfinished staging directories belong to a
            // previous interrupted process, never to a concurrent publication.
            if (entry.getName().endsWith(".tmp")) removeStaging(entry);
            else pending++;
        }
        if (pending >= MAX_PENDING) throw new IOException("Open or dismiss pending shared files first.");
        String id = String.format(Locale.ROOT, "%013d-%s", System.currentTimeMillis(), UUID.randomUUID());
        File staging = new File(inbox, id + ".tmp");
        if (!staging.mkdir()) throw new IOException("Cannot stage the shared file.");
        CancellationSignal cancellation = new CancellationSignal();
        AtomicReference<InputStream> stream = new AtomicReference<>();
        ScheduledFuture<?> timeout = TIMER.schedule(() -> {
            cancellation.cancel();
            try { InputStream in = stream.get(); if (in != null) in.close(); } catch (IOException ignored) {}
        }, 30, TimeUnit.SECONDS);
        try {
            String name = null;
            try (Cursor cursor = context.getContentResolver().query(uri,
                new String[]{OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE}, null, null, null, cancellation)) {
                if (cursor != null && cursor.moveToFirst()) {
                    int column = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME);
                    if (column >= 0) name = cursor.getString(column);
                    column = cursor.getColumnIndex(OpenableColumns.SIZE);
                    if (column >= 0 && !cursor.isNull(column) && cursor.getLong(column) > MAX_BYTES) {
                        throw new IOException("GPX exceeds 16 MiB.");
                    }
                }
            }
            name = filename(name, mime);
            try (android.content.res.AssetFileDescriptor descriptor =
                    context.getContentResolver().openAssetFileDescriptor(uri, "r", cancellation)) {
                if (descriptor == null) throw new IOException("The sending app did not provide a readable file.");
                try (InputStream in = descriptor.createInputStream();
                     OutputStream out = new FileOutputStream(new File(staging, "document.gpx"))) {
                    stream.set(in);
                    copyBounded(in, out);
                } finally { stream.set(null); }
            }
            cancellation.throwIfCanceled();
            Files.write(new File(staging, "metadata.json").toPath(),
                new JSONObject().put("filename", name).toString().getBytes(StandardCharsets.UTF_8));
            if (!staging.renameTo(new File(inbox, id))) throw new IOException("Cannot retain the shared file.");
        } finally {
            timeout.cancel(false);
            removeStaging(staging);
        }
    }

    static String filename(String name, String mime) throws IOException {
        if (name == null || name.isEmpty()) {
            if ("application/gpx+xml".equals(mime) || "application/gpx".equals(mime) || "application/x-gpx+xml".equals(mime)) {
                return "shared-track.gpx";
            }
            throw new IOException("The attachment must have a .gpx filename.");
        }
        if (name.startsWith(".") || name.contains("/") || name.contains("\\") || name.indexOf(0) >= 0 ||
            name.getBytes(StandardCharsets.UTF_8).length > 255 || !name.toLowerCase(Locale.ROOT).endsWith(".gpx")) {
            throw new IOException("The attachment must have a plain .gpx filename.");
        }
        return name;
    }

    static void copyBounded(InputStream in, OutputStream out) throws IOException {
        byte[] buffer = new byte[64 * 1024];
        long copied = 0;
        int count;
        while ((count = in.read(buffer)) != -1) {
            if (Thread.currentThread().isInterrupted()) throw new IOException("Shared file read timed out.");
            copied += count;
            if (copied > MAX_BYTES) throw new IOException("GPX exceeds 16 MiB.");
            out.write(buffer, 0, count);
        }
        if (copied == 0) throw new IOException("The shared file is empty.");
    }

    private static void removeStaging(File directory) {
        new File(directory, "document.gpx").delete();
        new File(directory, "metadata.json").delete();
        directory.delete();
    }
}
