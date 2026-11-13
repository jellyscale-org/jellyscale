import java.nio.file.*;
import kotlinx.serialization.KSerializer;
import kotlinx.serialization.builtins.BuiltinSerializersKt;
import kotlinx.serialization.json.*;

// java -cp 'jars/*' Decode.java <Model> <file> [<Model> <file>]...: decodes each file as
// org.jellyfin.sdk.model.api.<Model> ("Model[]" for a list of them) with the settings of
// jellyfin-sdk-kotlin's ApiSerializer, as Findroid does. A failure prints FAILED with the
// model, and the message of the serializer, which names the JSON path and the field.
public class Decode {
  public static void main(String[] args) throws Exception {
    Json json = JsonKt.Json(Json.Default, b -> {
      b.setLenient(false); b.setIgnoreUnknownKeys(true); b.setAllowSpecialFloatingPointValues(true);
      b.setExplicitNulls(false); b.setCoerceInputValues(true);
      return kotlin.Unit.INSTANCE;
    });
    int failed = 0;
    for (int i = 0; i + 1 < args.length; i += 2) {
      String name = args[i];
      try {
        boolean list = name.endsWith("[]");
        Class<?> model = Class.forName("org.jellyfin.sdk.model.api." + (list ? name.substring(0, name.length() - 2) : name));
        Object companion = model.getField("Companion").get(null);
        KSerializer<?> serializer = (KSerializer<?>) companion.getClass().getMethod("serializer").invoke(companion);
        if (list) serializer = BuiltinSerializersKt.ListSerializer(serializer);
        json.decodeFromString(serializer, Files.readString(Path.of(args[i + 1])));
        System.out.println("decoded " + name + " " + args[i + 1]);
      } catch (Exception e) {
        failed++;
        System.out.println("FAILED " + name + " " + args[i + 1] + ": " + e.getMessage());
      }
    }
    System.exit(failed > 0 || args.length < 2 ? 1 : 0);
  }
}
