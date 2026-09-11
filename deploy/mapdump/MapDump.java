import android.content.Context;
import android.content.ContextWrapper;

import java.util.Set;

/**
 * Reads one decrypted value out of MAP's credential store.
 *
 * map_data_storage_v2.db is encrypted at rest, so scanning it yields nothing.
 * Rather than reimplement the crypto, this loads MAP's own storage class and
 * lets it do the decryption. The lookups are plain map reads over state that
 * class decrypts when it loads, so no key handling happens here at all.
 * docs/command.md says which class, why the other one is wrong, and why the
 * context is wrapped.
 *
 * createPackageContext with INCLUDE_CODE gives a Context that both carries
 * MAP's data directory and loads MAP's classes, so the DB is found where MAP
 * expects it:
 *
 *   CLASSPATH=/data/local/map/mapdump.jar \
 *       app_process /data/local/map MapDump <key>
 *
 * stdout carries the value and nothing else -- one token, no newline, or
 * nothing at all. Every step, count and failure goes to stderr, so a caller can
 * log stderr freely and must never log stdout. Writing it to a file was the
 * older shape; a file outlives the process that asked for it.
 *
 * One caveat on that split: main throws, so an exception raised inside MAP's
 * own code reaches stderr with MAP's wording rather than ours. It names classes
 * and fields rather than values, but it is not text this file controls.
 */
public final class MapDump {

    private static final String MAP_PKG = "com.amazon.imp";

    private static final String STORAGE = "com.amazon.identity.auth.device.storage.BackwardsCompatiableDataStorage";
    private static final String MAP_CONTEXT = "com.amazon.identity.auth.device.framework.cb";

    public static void main(String[] args) throws Exception {
        if (args.length != 1) {
            System.err.println("usage: MapDump <tokenKey>");
            System.exit(2);
        }
        final String key = args[0];

        android.os.Looper.prepareMainLooper();

        Class<?> activityThread = Class.forName("android.app.ActivityThread");
        Object thread = activityThread.getMethod("systemMain").invoke(null);
        Context systemContext = (Context) activityThread.getMethod("getSystemContext").invoke(thread);
        System.err.println("step: system context = " + systemContext);

        Context packageContext = systemContext.createPackageContext(MAP_PKG,
                Context.CONTEXT_INCLUDE_CODE | Context.CONTEXT_IGNORE_SECURITY);

        Context map = new ContextWrapper(packageContext) {
            @Override
            public Context getApplicationContext() {
                return this;
            }
        };
        System.err.println("step: map context = " + map.getPackageName());

        ClassLoader classLoader = map.getClassLoader();
        Class<?> mapContext = classLoader.loadClass(MAP_CONTEXT);
        Object wrapped = mapContext.getMethod("X", Context.class).invoke(null, map);

        Class<?> storageClass = classLoader.loadClass(STORAGE);
        Object store = storageClass.getConstructor(mapContext).newInstance(wrapped);
        System.err.println("step: storage = " + store.getClass().getSimpleName());

        Object found = storageClass.getMethod("getAccounts").invoke(store);
        if (found == null) {
            System.err.println("accounts: 0 (getAccounts returned null)");
            System.exit(1);
        }
        Set<?> accounts = (Set<?>) found;
        System.err.println("accounts: " + accounts.size());

        java.util.List<String> ordered = new java.util.ArrayList<String>();
        for (Object account : accounts) {
            ordered.add(String.valueOf(account));
        }
        java.util.Collections.sort(ordered);

        java.lang.reflect.Method getValue = storageClass.getMethod("v", String.class, String.class);
        for (String account : ordered) {
            Object value = getValue.invoke(store, account, key);
            if (value == null) {
                System.err.println(key + " -> null");
                continue;
            }
            String token = (String) value;
            System.out.print(token);
            System.out.flush();
            System.err.println(key + " -> " + token.length() + " chars on stdout");
            System.exit(0);
        }
        System.err.println(key + " -> not found on any of " + accounts.size() + " accounts");
        System.exit(1);
    }
}
