package com.wails.app;

import org.junit.Test;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;

import static org.junit.Assert.*;

public class IncomingGpxTest {
    @Test public void keepsDisplayFilenameAndAcceptsGenericFileMime() throws Exception {
        assertEquals("Ruta Aragón.GPX", IncomingGpx.filename("Ruta Aragón.GPX", "application/octet-stream"));
        assertEquals("shared-track.gpx", IncomingGpx.filename(null, "application/gpx+xml"));
    }

    @Test public void refusesUnsafeOrNonGpxNames() {
        for (String name : new String[]{"../ride.gpx", "dir/ride.gpx", "dir\\ride.gpx", ".secret.gpx", "ride.txt", "bad\0.gpx", "a".repeat(256) + ".gpx"}) {
            assertThrows(IOException.class, () -> IncomingGpx.filename(name, "application/gpx+xml"));
        }
        assertThrows(IOException.class, () -> IncomingGpx.filename(null, "application/octet-stream"));
    }

    @Test public void copiesBytesWithoutRewritingXml() throws Exception {
        byte[] data = "<?xml version=\"1.0\"?><gpx>á &amp;</gpx>".getBytes(java.nio.charset.StandardCharsets.UTF_8);
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        IncomingGpx.copyBounded(new ByteArrayInputStream(data), out);
        assertArrayEquals(data, out.toByteArray());
    }

    @Test public void boundsTheStreamEvenWhenProviderSizeIsWrong() throws Exception {
        CountingOutput exact = new CountingOutput();
        IncomingGpx.copyBounded(zeros(IncomingGpx.MAX_BYTES), exact);
        assertEquals(IncomingGpx.MAX_BYTES, exact.count);
        CountingOutput oversized = new CountingOutput();
        assertThrows(IOException.class, () -> IncomingGpx.copyBounded(zeros(IncomingGpx.MAX_BYTES + 1), oversized));
        assertTrue(oversized.count <= IncomingGpx.MAX_BYTES);
        assertThrows(IOException.class, () -> IncomingGpx.copyBounded(zeros(0), new CountingOutput()));
    }

    private static InputStream zeros(long length) {
        return new InputStream() {
            long remaining = length;
            @Override public int read() { return remaining-- > 0 ? 0 : -1; }
            @Override public int read(byte[] bytes, int offset, int count) {
                if (remaining == 0) return -1;
                int n = (int)Math.min(remaining, count);
                remaining -= n;
                java.util.Arrays.fill(bytes, offset, offset + n, (byte)0);
                return n;
            }
        };
    }

    private static class CountingOutput extends OutputStream {
        long count;
        @Override public void write(int value) { count++; }
        @Override public void write(byte[] bytes, int offset, int length) { count += length; }
    }
}
